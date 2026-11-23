// Package daemon assembles a node: home directory, key, database, local API,
// peer listener and web UI. It also knows how to find or start the daemon
// from another process.
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
	Name      string   `json:"name,omitempty"`      // node name shown to peers; default: hostname
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

// Run is the daemon. It returns when ctx ends or a stop is requested.
func Run(ctx context.Context, home, version string) error {
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
	defer st.Close()
	n, err := node.New(ctx, st, id.NodeID(), cfg.Name, time.Now)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	var peerLn net.Listener
	if cfg.Listen != "" {
		if peerLn, err = net.Listen("tcp4", cfg.Listen); err != nil {
			return fmt.Errorf("peer listener: %w", err)
		}
	}
	addrs := func() []string { return advertised(cfg, peerLn) }
	mesh := node.NewMesh(n, peer.NewTransport(id), addrs)
	go mesh.Run(ctx)
	if peerLn != nil {
		refresh := func(room string) { go mesh.Refresh(ctx, room) }
		go func() {
			if err := peer.Serve(ctx, peerLn, id, n, refresh); err != nil && ctx.Err() == nil {
				slog.Error("peer listener stopped", "err", err)
			}
		}()
	}

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
		return fmt.Errorf("the UI must listen on a loopback address, not %s", uiLn.Addr())
	}
	uiURL := fmt.Sprintf("http://%s/#%s", uiLn.Addr(), token)
	uiSrv := &http.Server{Handler: web.Handler(n, mesh, token), ReadHeaderTimeout: 5 * time.Second}
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
	apiSrv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: localapi.Handler(n, mesh, localapi.Info{
		Version: version, Addrs: addrs, UIURL: func() string { return uiURL }, PID: os.Getpid(), Stop: stop,
	})}
	go apiSrv.Serve(apiLn)

	slog.Info("a2p2 daemon started", "node", core.ShortNode(n.ID), "name", cfg.Name, "home", home,
		"ui", uiLn.Addr().String(), "listen", addrs(), "version", version)
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(shut)
	_ = uiSrv.Shutdown(shut)
	slog.Info("a2p2 daemon stopped")
	return nil
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
