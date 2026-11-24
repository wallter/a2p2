package mcpshim

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/localapi"
)

// recorder is a local API socket in front of a real daemon: it records
// every call the shim makes, with its body, and passes it on.
type recorder struct {
	mu    sync.Mutex
	calls []string
	notes []string // status calls that asked to use up a person's notice
}

func record(t *testing.T, real string) (*localapi.Client, *recorder) {
	t.Helper()
	dir, err := os.MkdirTemp(sockTemp(), "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "rec")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	up := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", real)
	}}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		name := strings.TrimPrefix(r.URL.Path, "/v1/")
		rec.mu.Lock()
		rec.calls = append(rec.calls, name)
		if name == "status" && bytes.Contains(body, []byte("show_notice")) {
			rec.notes = append(rec.notes, string(body))
		}
		rec.mu.Unlock()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://a2p2"+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		// The local key (Windows): passed on as the shim sent it.
		if k := r.Header.Get(localapi.KeyHeader); k != "" {
			req.Header.Set(localapi.KeyHeader, k)
		}
		res, err := up.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	// The real daemon's key, where it wrote one (Windows), read at each
	// call as daemon.Client does.
	key := func() string {
		k, _ := os.ReadFile(filepath.Join(filepath.Dir(real), "local.key"))
		return strings.TrimSpace(string(k))
	}
	return localapi.NewClient(sock).WithKey(key), rec
}

// P3: driving every tool, with good and bad arguments, makes only the
// calls an agent may make; none reaches confirm, requests, a room change,
// net, alerts or stop, and no status call uses up a person's notice.
func TestEveryToolMakesOnlyAgentCalls(t *testing.T) {
	_, home := startDaemonHome(t)
	c, rec := record(t, daemon.SockPath(home))
	a := connect(t, c, "alice")
	b := connect(t, c, "bob")
	bob := agentID(t, b)
	out := ok(t, a, "send", map[string]any{"text": "hi", "to": []string{bob}})
	id := idOf(t, out)
	file := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(file, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // of "abc"
	for _, step := range []struct {
		cs   string
		tool string
		args map[string]any
	}{
		{"b", "inbox", map[string]any{}},
		{"b", "inbox", map[string]any{"thread": id}},
		{"b", "inbox", map[string]any{"recent": 5}},
		{"b", "inbox", map[string]any{"level": "none", "peek": true}},
		{"b", "inbox", map[string]any{"skipped": true}},
		{"b", "inbox", map[string]any{"level": "all"}},
		{"b", "reply", map[string]any{"id": id, "text": "ok"}},
		{"b", "ack", map[string]any{"id": id}},
		{"a", "ask_human", map[string]any{"question": "Ship?", "choices": []string{"yes", "no"}}},
		{"a", "ask_human", map[string]any{"question": "Choose a branch"}},
		{"a", "send", map[string]any{"text": "v2", "replaces": id}},
		{"a", "send", map[string]any{"text": "", "replaces": id}},
		{"a", "send", map[string]any{"text": "x", "room": "nope"}},
		{"a", "send", map[string]any{"text": "x", "reply_to": "nope", "ask_handled": true}},
		{"a", "ack", map[string]any{"id": "nope"}},
		{"a", "reply", map[string]any{"id": "nope", "text": "x"}},
		{"a", "inbox", map[string]any{"room": "nope", "wait_seconds": 1, "limit": 3}},
		{"a", "whoami", map[string]any{}},
		{"a", "peers", map[string]any{}},
		// files: a send with files only raises a request; fetch never
		// confirms, and nothing reaches the room's files setting
		{"a", "send", map[string]any{"text": "the log", "files": []string{file}}},
		{"a", "send", map[string]any{"text": "v3", "replaces": id, "files": []string{"sha256:" + sha}}},
		{"a", "send", map[string]any{"text": "x", "files": []string{"relative"}}},
		{"b", "inbox", map[string]any{"fetch": sha}},
		{"b", "inbox", map[string]any{"fetch": sha, "room": "local", "wait_seconds": 1}},
		{"b", "inbox", map[string]any{"fetch": "nothex"}},
		{"a", "inbox", map[string]any{}},
	} {
		cs := a
		if step.cs == "b" {
			cs = b
		}
		call(t, cs, step.tool, step.args)
	}
	allowed := []string{"register", "send", "inbox", "seen", "thread", "history", "ack", "status", "level", "unread", "fetch"}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) < 15 {
		t.Fatalf("recorded only %v", rec.calls)
	}
	for _, name := range rec.calls {
		if !slices.Contains(allowed, name) {
			t.Errorf("a tool made the local call %q", name)
		}
	}
	if len(rec.notes) != 0 {
		t.Errorf("a tool used up a person's notice: %v", rec.notes)
	}
}
