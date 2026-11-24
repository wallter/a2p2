package daemon

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// runKeyed runs a daemon on a new home that requires the local key, as on
// Windows, and returns the home and a function that stops it.
func runKeyed(t *testing.T) (string, func()) {
	t.Helper()
	home, err := os.MkdirTemp(sockTemp(), "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := SaveConfig(home, Config{UI: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan struct{})
	go func() { done <- RunWith(ctx, home, "test", RunOpts{LocalKey: true, Ready: func() { close(ready) }}) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon did not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not start")
	}
	stop := func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	return home, stop
}

func TestLocalAPICallWithoutTheLocalKeyIsRefusedBeforeAnyHandler(t *testing.T) {
	home, stop := runKeyed(t)
	defer stop()
	ctx := context.Background()
	for name, c := range map[string]*localapi.Client{
		"no key":    localapi.NewClient(SockPath(home)),
		"wrong key": localapi.NewClient(SockPath(home)).WithKey(func() string { return "00" }),
	} {
		for _, call := range []string{"status", "stop", "net"} {
			var e *localapi.Error
			if err := c.Call(ctx, call, nil, nil); !errors.As(err, &e) || e.Code != node.ErrorNotAllowed {
				t.Fatalf("%s %s: %v", name, call, err)
			}
		}
	}
	var st localapi.Status
	if err := Client(home).Call(ctx, "status", nil, &st); err != nil || st.Node == "" {
		t.Fatalf("with the key: %v %+v", err, st)
	}
}

func TestLocalKeyIsNewAtEachStartAndRemovedAtStop(t *testing.T) {
	home, stop := runKeyed(t)
	first := readLocalKey(home)
	stop()
	if _, err := os.Stat(localKeyPath(home)); !os.IsNotExist(err) {
		t.Fatalf("key file after stop: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan struct{})
	go func() { done <- RunWith(ctx, home, "test", RunOpts{LocalKey: true, Ready: func() { close(ready) }}) }()
	<-ready
	defer func() { cancel(); <-done }()
	second := readLocalKey(home)
	if first == "" || second == "" || first == second {
		t.Fatalf("keys %q then %q", first, second)
	}
	old := localapi.NewClient(SockPath(home)).WithKey(func() string { return first })
	if err := old.Call(ctx, "status", nil, nil); err == nil {
		t.Fatal("the previous run's key still works")
	}
}

func TestDaemonWithoutTheLocalKeyOptionWritesNoKeyOnUnix(t *testing.T) {
	if localKeyRequired {
		t.Skip("Windows always requires the local key")
	}
	m := newMachineWith(t, "a", nil)
	if _, err := os.Stat(localKeyPath(m.home)); !os.IsNotExist(err) {
		t.Fatalf("key file on Unix: %v", err)
	}
	if err := localapi.NewClient(SockPath(m.home)).Call(context.Background(), "status", nil, nil); err != nil {
		t.Fatal(err)
	}
}
