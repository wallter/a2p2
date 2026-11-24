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
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/daemon/portmap"
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
	if err := makeHome(home); err != nil {
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
	return checkPrivate(fi, home) // sys_unix.go, sys_windows.go
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

// advertised works out which addresses other nodes should dial; IPv4
// only for a decision recorded before IPv6 (DESIGN 11). An "advertise"
// entry the listener cannot serve (an IPv6 address under an IPv4-only
// decision or listener, an IPv4 one on an IPv6 listener) is left out and
// returned in dropped, with why.
func advertised(cfg Config, ln net.Listener, ipv4Only bool) (addrs, dropped []string) {
	if ln == nil {
		return nil, nil
	}
	tcp := ln.Addr().(*net.TCPAddr)
	if len(cfg.Advertise) > 0 {
		ip, _ := netip.AddrFromSlice(tcp.IP)
		ip = ip.Unmap()
		v4, v6 := !ip.IsUnspecified() && ip.Is4() || ipv4Only, !ip.IsUnspecified() && ip.Is6()
		for _, a := range cfg.Advertise {
			ap, err := netip.ParseAddrPort(a)
			switch {
			case err == nil && v4 && ap.Addr().Unmap().Is6() && !ap.Addr().Is4In6():
				dropped = append(dropped, a+" (an IPv6 address; the listener takes IPv4 only)")
			case err == nil && v6 && ap.Addr().Unmap().Is4():
				dropped = append(dropped, a+" (an IPv4 address; the listener takes IPv6 only)")
			default:
				addrs = append(addrs, a)
			}
		}
		return addrs, dropped
	}
	port := fmt.Sprint(tcp.Port)
	if tcp.IP.IsLoopback() {
		return []string{net.JoinHostPort(tcp.IP.String(), port)}, nil
	}
	if !tcp.IP.IsUnspecified() {
		return []string{net.JoinHostPort(tcp.IP.String(), port)}, nil
	}
	// Unspecified: one dual-stack socket; IPv4 and global IPv6 addresses
	// (DESIGN 11).
	return advertisable(localAddrs(), tcp.Port, ipv4Only), nil
}

// advert is what this run advertises: the router's mapped address while
// a member has recently reached it first, then the interface addresses,
// at most node.MaxAddrs. Nothing members say they see is ever in it.
type advert struct {
	base     []string
	verified func() []string
}

func (a *advert) now() []string {
	var out []string
	var v []string
	if a.verified != nil {
		v = a.verified()
	}
	for _, x := range append(v, a.base...) {
		if !slices.Contains(out, x) && len(out) < node.MaxAddrs {
			out = append(out, x)
		}
	}
	return out
}

// RunOpts are for tests: what Run takes from its caller instead of making.
type RunOpts struct {
	// Listen, if set, makes the peer listener of each run with networking
	// on, instead of listening on the decided address.
	Listen func(addr string) (net.Listener, error)
	// Ready, if set, is called once every listener serves, the first time.
	Ready func()
	// Gateway, if set, replaces reading the default gateway and this
	// machine's address on its network, for port mapping; Mapper, if set,
	// replaces the client that talks to it. Tests give fake routers on
	// loopback here: nothing in a test may reach a real router.
	Gateway func() (gw, local netip.Addr, err error)
	Mapper  func(gw, local netip.Addr) portmap.Mapper
	// Exchange, if set, is given each run's pull exchange once it exists.
	Exchange func(*node.Exchange)
	// Handler, if set, is called as a local API or page request enters
	// its handler (enter) and as it leaves.
	Handler func(path string, enter bool)
	// LocalKey requires the local key (localkey.go) as on Windows.
	LocalKey bool
}

// Run is the daemon. It returns when ctx ends or a stop is requested.
func Run(ctx context.Context, home, version string) error {
	return RunWith(ctx, home, version, RunOpts{})
}

// CheckUIAddr accepts only a literal loopback address with a port, such as
// 127.0.0.1:7422 or [::1]:7422: it is checked before anything is bound or
// any name is looked up, so the page is never reachable from elsewhere.
func CheckUIAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	if err != nil || port == "" || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("the page listens only on a loopback IP address with a port, such as %s; not %q "+
			"(fix \"ui\" in config.json)", DefaultUI, addr)
	}
	return nil
}

// RunWith is Run with test options. The home's lock, key, database, the
// page's listener and token, and the local socket last the whole process.
// The machine itself (rooms, surfaces, pull and join, and networking) is a
// run inside it, started again in place when a person records a
// networking decision, so the decision applies at once whether or not the
// command that recorded it is still there.
func RunWith(ctx context.Context, home, version string, o RunOpts) (err error) {
	if err := PrepareHome(home); err != nil {
		return err
	}
	// One daemon per home: hold an exclusive lock for the process lifetime.
	lock, err := os.OpenFile(filepath.Join(home, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := tryLock(lock); err != nil {
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
	uiAddr := cfg.UI
	if uiAddr == "" {
		uiAddr = DefaultUI
	}
	if err := CheckUIAddr(uiAddr); err != nil {
		return err
	}
	umask(0o077) // everything the daemon creates is private to this user
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

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// The handlers of the current run; swapped when a run starts.
	var cur atomic.Pointer[handlers]
	// Between runs (start, stop, a networking decision being applied) the
	// local API refuses everything except stop, which must never be lost.
	between := &handlers{ui: stopping, api: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/stop" {
			stop()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}\n"))
			return
		}
		stopping(w, r)
	})}
	cur.Store(between)
	var servers []*http.Server
	defer func() {
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shut)
			srv.Close() // waiting reads that outlived the grace period
		}
		slog.Info("a2p2 daemon stopped")
	}()
	base := func(net.Listener) context.Context { return ctx } // requests end when the daemon stops

	// Web UI: loopback only (checked above), token required. If the port
	// is taken, any port on the same address.
	token, err := newToken()
	if err != nil {
		return err
	}
	uiLn, err := net.Listen("tcp", uiAddr)
	if err != nil {
		host, _, _ := net.SplitHostPort(uiAddr)
		if uiLn, err = net.Listen("tcp", net.JoinHostPort(host, "0")); err != nil {
			return fmt.Errorf("ui listener: %w", err)
		}
	}
	uiURL := fmt.Sprintf("http://%s/#%s", uiLn.Addr(), token)
	uiSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cur.Load().ui.ServeHTTP(w, r) }),
		ReadHeaderTimeout: 5 * time.Second, BaseContext: base}
	servers = append(servers, uiSrv)
	go uiSrv.Serve(uiLn)

	// Local API on the Unix socket. We hold the lock, so a leftover socket
	// file is stale and safe to remove.
	sock := sockPath(home)
	_ = os.Remove(sock)
	var api http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cur.Load().api.ServeHTTP(w, r) })
	_ = os.Remove(localKeyPath(home))
	if localKeyRequired || o.LocalKey { // Windows: localkey.go
		key, err := writeLocalKey(home)
		if err != nil {
			return fmt.Errorf("local key: %w", err)
		}
		defer os.Remove(localKeyPath(home))
		api = requireKey(key, api)
	}
	apiLn, err := listenLocal(sock)
	if err != nil {
		return fmt.Errorf("local socket: %w", err)
	}
	defer os.Remove(sock)
	apiSrv := &http.Server{ReadHeaderTimeout: 5 * time.Second, BaseContext: base, Handler: api}
	servers = append(servers, apiSrv)
	go apiSrv.Serve(apiLn)

	ready := o.Ready
	for {
		restart := make(chan struct{}, 1)
		r := &run{home: home, version: version, cfg: cfg, id: id, st: st, o: o, cur: &cur, token: token,
			uiURL: uiURL, uiAddr: uiLn.Addr().String(), stop: stop, restart: restart, between: between}
		again, err := r.serve(ctx, ready)
		if err != nil || !again || ctx.Err() != nil {
			return err
		}
		ready = nil
		slog.Info("a2p2 daemon applies a networking decision")
	}
}

// handlers serve the page and the local API for one run.
type handlers struct{ ui, api http.Handler }

var stopping = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "the a2p2 daemon is starting or stopping; try again", http.StatusServiceUnavailable)
})

// run is one run of the machine inside the daemon process.
type run struct {
	home, version string
	cfg           Config
	id            *peer.Identity
	st            *store.Store
	o             RunOpts
	cur           *atomic.Pointer[handlers]
	token, uiURL  string
	uiAddr        string
	stop          func()
	between       *handlers // served while no run is serving
	restart       chan struct{}

	mu    sync.Mutex
	fault string // why networking on does not listen; empty when it does
}

func (r *run) setFault(s string) {
	r.mu.Lock()
	r.fault = s
	r.mu.Unlock()
}

func (r *run) getFault() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fault
}

// serve runs the machine until the daemon stops (false) or a networking
// decision is recorded (true: run again to apply it).
func (r *run) serve(parent context.Context, ready func()) (again bool, err error) {
	st, o := r.st, r.o
	// Records, the room log, pull and join (DESIGN sections 2 to 4 and 7).
	m := node.NewMachine(st, r.id.Key, node.MachineOpts{})
	rooms, err := node.OpenRooms(parent, m)
	if err != nil {
		return false, err
	}
	if err := m.Prune(parent); err != nil {
		return false, err
	}
	// Networking is what a person last decided (a2p2 net, recorded in
	// requests); with no decision it is off. The config file's listen
	// address is only the default a2p2 net on offers. An address that
	// cannot be bound leaves the machine running, connecting out, and
	// saying why it does not listen.
	netw, err := node.LoadNetwork(parent, st)
	if err != nil {
		return false, err
	}
	var peerLn net.Listener
	if netw.State == node.NetworkOn {
		// "tcp": an unspecified host gives one socket for IPv4 and IPv6; a
		// decision from before IPv6 keeps its IPv4 scope ("tcp4").
		family := "tcp"
		if netw.IPv4Only {
			family = "tcp4"
		}
		listen := func(addr string) (net.Listener, error) { return net.Listen(family, addr) }
		if o.Listen != nil {
			listen = o.Listen
		}
		if peerLn, err = listen(netw.Listen); err != nil {
			r.setFault("not listening: " + err.Error())
			slog.Error("peer listener", "listen", netw.Listen, "err", err)
			peerLn = nil
		}
	}
	defer func() {
		if peerLn != nil {
			peerLn.Close()
		}
	}()
	addrs, dropped := advertised(r.cfg, peerLn, netw.IPv4Only)
	if len(dropped) > 0 {
		slog.Warn("not advertised", "addrs", strings.Join(dropped, "; "))
	}
	if len(addrs) > node.MaxAddrs {
		addrs = addrs[:node.MaxAddrs]
	}
	// Peers' addresses as last stored (peer_addrs): a machine that does
	// not listen can reach its peers again after a restart. What joins,
	// invites and pulls teach the book it saves itself.
	book, err := node.LoadAddrBook(parent, st)
	if err != nil {
		return false, err
	}
	usable := liveUsable() // unique-local IPv6 only within its own site, on every dial path
	pulls := peer.NewPullClient(r.id, book)
	pulls.Usable = usable
	defer pulls.Close()
	// Port mapping, when a person asked for it (a2p2 net on --map-port),
	// and the removal of any an earlier run left (DESIGN 11).
	var reach *node.Reacher
	keeper, mapErr := r.keeper(netw, peerLn, func() {
		if reach != nil {
			reach.Nudge()
		}
	})
	adv := &advert{base: addrs}
	x := node.NewExchange(rooms, pulls, node.ExchangeOpts{Book: book, Addrs: addrs, AddrsNow: adv.now})
	files := node.NewFiles(rooms, peer.NewFileClient(pulls), node.FilesOpts{})
	x.UseFiles(files)
	if err := files.Start(parent); err != nil {
		return false, err
	}
	if o.Exchange != nil {
		o.Exchange(x)
	}
	jc := peer.NewJoinClient(r.id)
	jc.Usable = usable
	joiner := node.NewJoiner(rooms, jc, node.JoinerOpts{Name: r.cfg.Name, Addrs: addrs})
	// Reachability (PROTOCOL 4.7): the prober tries members' addresses for
	// a2p2 net check and dials back for members that ask; a run that
	// listens asks members to dial back to it.
	prober := peer.NewProber(r.id)
	prober.Usable = usable
	if peerLn != nil {
		reach = node.NewReacher(pulls, node.ReachOpts{Members: rooms.OtherMembers, Book: book,
			Addrs: func() []string { return addrs }, Candidates: func() []string { return mappedAddr(keeper) }})
		adv.verified = reach.Verified
	}
	surf, err := node.OpenSurface(parent, m, rooms, node.SurfaceOpts{Name: r.cfg.Name, Joiner: joiner, Book: book,
		Addrs: adv.now, Network: netw, NetworkFault: r.getFault, NotMember: x.NotMemberSince,
		Probe: prober, Reach: reach, Mapping: func() *node.PortMapping { return mappingView(keeper, mapErr) },
		NotAdvertised: dropped,
		OnNetwork: func(node.Network) {
			select {
			case r.restart <- struct{}{}:
			default:
			}
		}})
	if err != nil {
		return false, err
	}

	ctx, stop := context.WithCancel(parent)
	defer stop()
	// gate lets every request handler of this run finish before the run
	// ends (and the store closes after the last), and refuses the ones
	// that start after; a handler's request ends when the run does.
	g := &gate{ctx: ctx, hook: o.Handler}
	var wg sync.WaitGroup
	start := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	defer func() {
		// Held (waiting) pulls end at once: peer.Serve cancels every
		// request in progress when ctx ends, before its grace period.
		stop()
		r.cur.Store(r.between)
		g.close()
		wg.Wait()
	}()
	keeping := false
	if peerLn != nil {
		h := filesPeerHandler{peerHandler{JoinServer: node.NewJoinServer(rooms), x: x, g: g, db: node.NewDialBackServer(rooms, prober), reach: reach}, files}
		ln := peerLn
		peerLn = nil // Serve owns it now
		lnCtx, lnStop := context.WithCancel(ctx)
		if keeper != nil && netw.MapPort {
			// The mapping lives exactly as long as the listener it forwards
			// to, however that ends.
			keeping = true
			start(func() { keeper.Run(lnCtx) })
		}
		start(func() {
			defer lnStop()
			if err := peer.Serve(ctx, ln, r.id, h); err != nil && ctx.Err() == nil {
				r.setFault("not listening: the listener stopped: " + err.Error())
				slog.Error("peer listener stopped", "err", err)
			}
		})
	}
	if netw.State != node.NetworkOff { // off: no scheduler, no join polling, nothing dialled
		start(func() { node.NewScheduler(x, node.SchedOpts{}).Run(ctx) })
		start(func() { resumeJoins(ctx, surf) })
		start(func() { files.Run(ctx, time.Minute) })
	}
	if reach != nil {
		start(func() { reach.Run(ctx) })
	}
	if keeper != nil && !keeping { // only removes what an earlier run left
		start(func() { keeper.Run(ctx) })
	}
	start(func() { prune(ctx, m) })
	r.cur.Store(&handlers{
		ui: g.wrap(web.Handler(surf, r.token)),
		api: g.wrap(localapi.Handler(surf, localapi.Info{
			Version: r.version, UIURL: func() string { return r.uiURL }, PID: os.Getpid(), Stop: r.stop,
		})),
	})
	slog.Info("a2p2 daemon started", "node", core.ShortNode(m.ID), "name", r.cfg.Name, "home", r.home,
		"ui", r.uiAddr, "network", netw.State, "listen", addrs, "fault", r.getFault(), "version", r.version)
	if ready != nil {
		ready()
	}
	select {
	case <-ctx.Done():
		return false, nil
	case <-r.restart:
		return true, nil
	}
}

// keeper prepares this run's port mapping, or only the removal of what
// an earlier run left (internal port 0), or says why there is neither.
// A mapping is made only under a decision with --map-port, for a listener
// bound to the wildcard address or to this machine's address on the
// gateway's network: never for one that would forward to another service.
// changed is called when the mapped address changes.
func (r *run) keeper(netw node.Network, ln net.Listener, changed func()) (*portmap.Keeper, string) {
	// A record that cannot be used whole stops everything: nothing is sent
	// to the router, the file stays, and status says what to remove by hand.
	path := filepath.Join(r.home, "portmap.json")
	left, err := (&portmap.File{Path: path}).Load()
	if err != nil {
		return nil, portmap.UnusableText(err)
	}
	want := netw.MapPort && ln != nil
	if !want && len(left) == 0 {
		return nil, ""
	}
	gwf := gatewayLocal
	if r.o.Gateway != nil {
		gwf = r.o.Gateway
	}
	gw, local, err := gwf()
	if err != nil {
		return nil, err.Error()
	}
	rec := &portmap.File{Path: path, Gateway: gw}
	if _, err := rec.Load(); err != nil {
		return nil, portmap.UnusableText(err)
	}
	var m portmap.Mapper = &portmap.Client{Gateway: gw, Local: local}
	if r.o.Mapper != nil {
		m = r.o.Mapper(gw, local)
	}
	port := uint16(0)
	why := ""
	if want {
		tcp := ln.Addr().(*net.TCPAddr)
		ip, _ := netip.AddrFromSlice(tcp.IP)
		if ip = ip.Unmap(); ip.IsUnspecified() || ip == local {
			port = uint16(tcp.Port)
		} else {
			why = "not mapped: the listener is on " + ip.String() + ", but the router would forward to " +
				local.String() + "; listen on 0.0.0.0 or on " + local.String()
		}
	}
	if port == 0 && len(left) == 0 {
		return nil, why
	}
	last := ""
	k := portmap.NewKeeper(m, port, rec, func(st portmap.State) {
		if st.Err != "" {
			slog.Warn("port mapping", "err", st.Err)
		}
		if len(st.Left) > 0 {
			slog.Warn("port mapping not removed", "left", portmap.LeftText(st.Left))
		}
		ext := ""
		if st.Mapping != nil {
			ext = st.Mapping.External.String()
			if ext != last {
				slog.Info("port mapping", "protocol", st.Mapping.Protocol, "external", ext,
					"lifetime", st.Mapping.Lifetime.String())
			}
		}
		if ext != last {
			last = ext
			changed()
		}
	})
	return k, why
}

// mappedAddr is the mapped external address, to verify by a dial-back
// before it is advertised: none once the lease has run out, whether or
// not a renewal is still being tried.
func mappedAddr(k *portmap.Keeper) []string {
	return mappedAt(k, time.Now())
}

func mappedAt(k *portmap.Keeper, now time.Time) []string {
	if k == nil {
		return nil
	}
	if m := k.State().Mapping; m != nil && m.External.Addr().IsValid() && m.External.Port() != 0 && m.Live(now) {
		return []string{m.External.String()}
	}
	return nil
}

// mappingView is the mapping as status and net check show it: nil when
// there is nothing to say.
func mappingView(k *portmap.Keeper, why string) *node.PortMapping {
	if k == nil {
		if why == "" {
			return nil
		}
		return &node.PortMapping{Error: why}
	}
	st := k.State()
	v := &node.PortMapping{NotRemoved: st.Left}
	switch {
	case st.Mapping != nil:
		v.Protocol, v.External, v.Error = st.Mapping.Protocol, st.Mapping.External.String(), st.Err
		if !st.Mapping.Expires.IsZero() && st.Mapping.Lifetime > 0 {
			v.Expires = st.Mapping.Expires.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	case st.Err != "":
		v.Error = st.Err
	case why != "":
		v.Error = why
	case len(st.Left) == 0:
		return nil // not tried yet, or nothing left
	}
	return v
}

// gate counts request handlers in flight. close waits for them and makes
// every later one fail, so that none touches the store after it closes.
type gate struct {
	ctx    context.Context // the run's: a request in flight ends with it
	hook   func(path string, enter bool)
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
		if g.hook != nil {
			g.hook(r.URL.Path, true)
			defer g.hook(r.URL.Path, false)
		}
		if g.ctx != nil {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			defer context.AfterFunc(g.ctx, cancel)()
			r = r.WithContext(ctx)
		}
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
		// Withdrawn and expired text leaves the store's files within a
		// minute (requirement D9), not only its tables.
		if err := m.Scrub(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("scrubbing", "err", err)
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
	x     *node.Exchange
	g     *gate
	db    *node.DialBackServer
	reach *node.Reacher
}

// Inbound records a connection in for "reachable by peers" (DESIGN 11).
func (h peerHandler) Inbound(from, local string) {
	if h.reach != nil {
		h.reach.ConnectedIn(from, local)
	}
}

func (h peerHandler) DialBack(ctx context.Context, from, source string, q *node.DialBackRequest) (*node.DialBackResponse, *node.PullError) {
	if !h.g.enter() {
		return nil, &node.PullError{Status: http.StatusServiceUnavailable, Code: node.CodeUnavailable, RetryAfter: time.Second}
	}
	defer h.g.leave()
	return h.db.DialBack(ctx, from, source, q)
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
	c := Client(home)
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
	if cmd, err = startDetached(cmd); err != nil { // survive the caller's terminal

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
		if tryLock(f) == nil {
			unlock(f)
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
	c := Client(home)
	return c, CheckHome(home) == nil && ping(ctx, c) == nil
}
