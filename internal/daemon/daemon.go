// Package daemon assembles a machine: home directory, key, database, the
// next version's path (records, room log, pull, join), local API, peer
// listener and web UI. It also knows how to find or start the daemon from
// another process.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/peer"
	"github.com/wallter/a2p2/internal/store"
	"github.com/wallter/a2p2/internal/web"
)

// Config is home/config.json. Everything is optional.
type Config struct {
	Name      string   `json:"name,omitempty"`      // machine name shown to peers; default: hostname
	Listen    string   `json:"listen,omitempty"`    // peer listener, e.g. "0.0.0.0:7423"; empty = networking off
	Advertise []string `json:"advertise,omitempty"` // addresses to give peers instead of detected ones
	UI        string   `json:"ui,omitempty"`        // web UI address; default 127.0.0.1:7422
}

const (
	DefaultUI     = "127.0.0.1:7422"
	DefaultListen = "0.0.0.0:7423"
)

// Home returns the state directory: $A2P2_HOME or ~/.a2p2.
func Home() string {
	if h := os.Getenv("A2P2_HOME"); h != "" {
		return h
	}
	d, err := os.UserHomeDir()
	if err != nil {
		return ".a2p2"
	}
	return filepath.Join(d, ".a2p2")
}

// PrepareHome creates the state directory if needed and refuses to use one
// that other users could read or that belongs to someone else: the socket in
// it grants full control of the node.
func PrepareHome(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return CheckHome(home)
}

// CheckHome verifies an existing state directory without creating anything.
// Clients call it before dialing the socket inside: a socket in a directory
// someone else controls could be an impostor that reads what we send it.
func CheckHome(home string) error {
	fi, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (symlinks are not followed)", home)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user", home)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other users (mode %o); run: chmod 700 %s", home, fi.Mode().Perm(), home)
	}
	return nil
}

func LoadConfig(home string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(home, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	return c, nil
}

func SaveConfig(home string, c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(home, "config.json"), append(b, '\n'), 0o600)
}

func sockPath(home string) string { return filepath.Join(home, "sock") }

// newToken returns a fresh random UI token. It is never stored or reused: a
// browser tab left open keeps sending the old token to the old port, and
// after the daemon stops another local user could be listening there.
func newToken() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// advertised works out which addresses other nodes should dial.
func advertised(cfg Config, ln net.Listener) []string {
	if ln == nil {
		return nil
	}
	if len(cfg.Advertise) > 0 {
		return cfg.Advertise
	}
	tcp := ln.Addr().(*net.TCPAddr)
	port := fmt.Sprint(tcp.Port)
	if tcp.IP.IsLoopback() {
		return []string{net.JoinHostPort("127.0.0.1", port)}
	}
	if !tcp.IP.IsUnspecified() {
		return []string{net.JoinHostPort(tcp.IP.String(), port)}
	}
	out := []string{}
	ifaces, _ := net.InterfaceAddrs()
	for _, a := range ifaces {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, net.JoinHostPort(ipn.IP.String(), port))
		}
	}
	return out
}

// RunOpts are for tests: what Run takes from its caller instead of making.
type RunOpts struct {
	// PeerListener, if set, is used as the peer listener when networking
	// is on, instead of listening on the decided address. Run closes it.
	PeerListener net.Listener
	// Ready, if set, is called once every listener serves.
	Ready func()
	// Exchange, if set, is given the pull exchange once it exists.
	Exchange func(*node.Exchange)
}

// Run is the daemon. It returns when ctx ends or a stop is requested.
func Run(ctx context.Context, home, version string) error {
	return RunWith(ctx, home, version, RunOpts{})
}

// RunWith is Run with test options.
func RunWith(ctx context.Context, home, version string, o RunOpts) (err error) {
	peerLn := o.PeerListener
	defer func() {
		if peerLn != nil && err != nil {
			peerLn.Close()
		}
	}()
	if err := PrepareHome(home); err != nil {
		return err
	}
	// One daemon per home: hold an exclusive lock for the process lifetime.
	lock, err := os.OpenFile(filepath.Join(home, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another a2p2 daemon is already running for %s", home)
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		return err
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	cfg.Name, _, _ = strings.Cut(cfg.Name, ".") // "host.local" -> "host"
	cfg.Name = core.CleanLabel(cfg.Name)
	syscall.Umask(0o077) // everything the daemon creates is private to this user
	id, err := peer.LoadIdentity(home)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(home, "a2p2.db"))
	if err != nil {
		return err
	}
	// Closed last of all, after every handler and loop has returned.
	defer st.Close()

	// Records, the room log, pull and join (DESIGN-next section 9).
	m := node.NewMachine(st, id.Key, node.MachineOpts{})
	rooms, err := node.OpenRooms(ctx, m)
	if err != nil {
		return err
	}
	if err := m.Prune(ctx); err != nil {
		return err
	}
	// Networking is what a person last decided (a2p2 net, recorded in
	// requests); with no decision it is off. The config file's listen
	// address is only the default a2p2 net on offers.
	netw, err := node.LoadNetwork(ctx, st)
	if err != nil {
		return err
	}
	if netw.State != node.NetworkOn {
		if peerLn != nil {
			peerLn.Close()
		}
		peerLn = nil
	} else if peerLn == nil {
		if peerLn, err = net.Listen("tcp4", netw.Listen); err != nil {
			return fmt.Errorf("peer listener on %s: %w", netw.Listen, err)
		}
	}
	addrs := advertised(cfg, peerLn)
	if len(addrs) > node.MaxAddrs {
		addrs = addrs[:node.MaxAddrs]
	}
	// Peers' addresses as last stored (peer_addrs): a machine that does
	// not listen can reach its peers again after a restart. What joins,
	// invites and pulls teach the book it saves itself.
	book, err := node.LoadAddrBook(ctx, st)
	if err != nil {
		return err
	}
	pulls := peer.NewPullClient(id, book)
	defer pulls.Close()
	x := node.NewExchange(rooms, pulls, node.ExchangeOpts{Book: book, Addrs: addrs})
	if o.Exchange != nil {
		o.Exchange(x)
	}
	joiner := node.NewJoiner(rooms, peer.NewJoinClient(id), node.JoinerOpts{Name: cfg.Name, Addrs: addrs})
	surf, err := node.OpenSurface(ctx, m, rooms, node.SurfaceOpts{Name: cfg.Name, Joiner: joiner, Book: book,
		Addrs: func() []string { return addrs }, Network: netw})
	if err != nil {
		return err
	}
	if n, err := surf.Notice(ctx, false); err == nil && n != "" {
		slog.Warn(n) // not recorded as shown: a person sees it through status or the page
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// gate lets every request handler finish before the store closes, and
	// refuses the ones that start after.
	g := &gate{}
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	if peerLn != nil {
		h := peerHandler{JoinServer: node.NewJoinServer(rooms), x: x, g: g}
		ln := peerLn
		peerLn = nil // Serve owns it now
		run(func() {
			if err := peer.Serve(ctx, ln, id, h); err != nil && ctx.Err() == nil {
				slog.Error("peer listener stopped", "err", err)
			}
		})
	}
	if netw.State != node.NetworkOff { // off: no scheduler, no join polling, nothing dialled
		run(func() { node.NewScheduler(x, node.SchedOpts{}).Run(ctx) })
		run(func() { resumeJoins(ctx, surf) })
	}
	run(func() { prune(ctx, m) })
	// shutdown, deferred before the listeners below: cancel every request
	// and loop, drain the handlers, wait for the loops; the store closes
	// after (deferred first).
	var servers []*http.Server
	defer func() {
		// Held (waiting) pulls end at once: peer.Serve cancels every
		// request in progress when ctx ends, before its grace period.
		stop()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shut)
			srv.Close() // waiting reads that outlived the grace period
		}
		g.close()
		wg.Wait()
		slog.Info("a2p2 daemon stopped")
	}()
	base := func(net.Listener) context.Context { return ctx } // requests end when the daemon stops

	// Web UI: loopback only, token required. If the port is taken, pick any.
	token, err := newToken()
	if err != nil {
		return err
	}
	uiAddr := cfg.UI
	if uiAddr == "" {
		uiAddr = DefaultUI
	}
	uiLn, err := net.Listen("tcp4", uiAddr)
	if err != nil {
		if uiLn, err = net.Listen("tcp4", "127.0.0.1:0"); err != nil {
			return fmt.Errorf("ui listener: %w", err)
		}
	}
	if !uiLn.Addr().(*net.TCPAddr).IP.IsLoopback() {
		uiLn.Close()
		return fmt.Errorf("the UI must listen on a loopback address, not %s", uiLn.Addr())
	}
	uiURL := fmt.Sprintf("http://%s/#%s", uiLn.Addr(), token)
	uiSrv := &http.Server{Handler: g.wrap(web.Handler(surf, token)), ReadHeaderTimeout: 5 * time.Second, BaseContext: base}
	servers = append(servers, uiSrv)
	go uiSrv.Serve(uiLn)

	// Local API on the Unix socket. We hold the lock, so a leftover socket
	// file is stale and safe to remove.
	sock := sockPath(home)
	_ = os.Remove(sock)
	old := syscall.Umask(0o177)
	apiLn, err := net.Listen("unix", sock)
	syscall.Umask(old)
	if err != nil {
		return fmt.Errorf("local socket: %w", err)
	}
	defer os.Remove(sock)
	apiSrv := &http.Server{ReadHeaderTimeout: 5 * time.Second, BaseContext: base, Handler: g.wrap(localapi.Handler(surf, localapi.Info{
		Version: version, UIURL: func() string { return uiURL }, PID: os.Getpid(), Stop: stop,
	}))}
	servers = append(servers, apiSrv)
	go apiSrv.Serve(apiLn)

	slog.Info("a2p2 daemon started", "node", core.ShortNode(m.ID), "name", cfg.Name, "home", home,
		"ui", uiLn.Addr().String(), "network", netw.State, "listen", addrs, "version", version)
	if o.Ready != nil {
		o.Ready()
	}
	<-ctx.Done()
	return nil
}

// gate counts request handlers in flight. close waits for them and makes
// every later one fail, so that none touches the store after it closes.
type gate struct {
	mu     sync.RWMutex
	closed bool
}

func (g *gate) enter() bool {
	g.mu.RLock()
	if g.closed {
		g.mu.RUnlock()
		return false
	}
	return true
}

func (g *gate) leave() { g.mu.RUnlock() }

func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

func (g *gate) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.enter() {
			http.Error(w, "the a2p2 daemon is stopping", http.StatusServiceUnavailable)
			return
		}
		defer g.leave()
		h.ServeHTTP(w, r)
	})
}

// errStopping answers a peer while the daemon stops.
var errStopping = errors.New("the a2p2 daemon is stopping")

// resumeJoins follows joins waiting for the admin machine's person: at
// start, then every node.JoinRetry, one poll each.
func resumeJoins(ctx context.Context, s *node.Surface) {
	for {
		if _, err := s.ResumeJoins(ctx, 0); err != nil && ctx.Err() == nil {
			slog.Warn("resuming joins", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(node.JoinRetry):
		}
	}
}

// prune runs the pruning pass hourly, and within a minute of a read
// asking for it.
func prune(ctx context.Context, m *node.Machine) {
	last := time.Now()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if m.PruneDue() || time.Since(last) >= time.Hour {
			last = time.Now()
			if err := m.Prune(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("pruning", "err", err)
			}
		}
	}
}

// peerHandler serves the peer routes, /p/v2/join and /p/v2/pull, each
// call inside the gate.
type peerHandler struct {
	*node.JoinServer
	x *node.Exchange
	g *gate
}

func (h peerHandler) Serve(ctx context.Context, from string, q *node.PullRequest) (*node.PullResponse, error) {
	if !h.g.enter() {
		return nil, errStopping
	}
	defer h.g.leave()
	return h.x.Serve(ctx, from, q)
}

func (h peerHandler) Handle(ctx context.Context, from, source string, body []byte) node.JoinReply {
	if !h.g.enter() {
		return node.JoinReply{Status: http.StatusServiceUnavailable, RetryAfter: node.JoinRetry}
	}
	defer h.g.leave()
	return h.JoinServer.Handle(ctx, from, source, body)
}

// Connect returns a client for the daemon of home, starting the daemon in the
// background if it is not running.
func Connect(ctx context.Context, home string) (*localapi.Client, error) {
	if err := PrepareHome(home); err != nil {
		return nil, err
	}
	c := localapi.NewClient(sockPath(home))
	if ping(ctx, c) == nil {
		return c, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(home, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Env = append(os.Environ(), "A2P2_HOME="+home)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the caller's terminal
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	go cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ping(ctx, c) == nil {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("the a2p2 daemon did not start; see %s", filepath.Join(home, "daemon.log"))
}

// WaitStopped waits up to d for the daemon of home to have exited: its
// lock is free. It reports whether it did.
func WaitStopped(home string, d time.Duration) bool {
	f, err := os.OpenFile(filepath.Join(home, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	for deadline := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// SockPath is the local API socket of home.
func SockPath(home string) string { return sockPath(home) }

func ping(ctx context.Context, c *localapi.Client) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return c.Call(ctx, "status", nil, nil)
}

// Running reports whether a daemon is serving home, without starting one.
func Running(ctx context.Context, home string) (*localapi.Client, bool) {
	c := localapi.NewClient(sockPath(home))
	return c, CheckHome(home) == nil && ping(ctx, c) == nil
}
