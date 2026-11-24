package mcpshim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// L1, P3: send with files only raises a request, which no tool can
// approve; after the person approves, a reader's inbox lists the file and
// inbox fetch returns its state and a path, never its bytes; the agent
// that asked is told it was sent; a replacement without files says so.
func TestASendWithFilesWaitsForThePersonAndFetchReturnsAPathNotBytes(t *testing.T) {
	c := startDaemon(t)
	a, b := connect(t, c, "alice"), connect(t, c, "bob")
	agentID(t, b)
	body := "secret build output 12345"
	file := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(h[:])

	var w struct {
		Request string              `json:"request"`
		State   string              `json:"state"`
		Files   []localapi.FileInfo `json:"files"`
		Note    string              `json:"note"`
	}
	out := ok(t, a, "send", map[string]any{"text": "the log", "files": []string{file}})
	if err := json.Unmarshal([]byte(out), &w); err != nil || w.State != "waiting" || w.Request == "" || len(w.Files) != 1 ||
		w.Files[0].SHA256 != sha || !strings.Contains(w.Note, "nothing is sent until then") {
		t.Fatalf("send with files = %s", out)
	}
	if s := ok(t, b, "inbox", nil); !isEmpty(t, s) {
		t.Fatalf("a file was sent before the person approved: %s", s)
	}
	// the person, on this machine
	var cr localapi.ConfirmResp
	if err := c.Call(context.Background(), "confirm", localapi.ConfirmReq{ID: w.Request, Approve: true}, &cr); err != nil || cr.Effect == "" {
		t.Fatalf("confirm: %+v %v", cr, err)
	}
	es := parseEntries(t, ok(t, b, "inbox", nil))
	if len(es) != 1 || es[0].ID != cr.Effect || len(es[0].Files) != 1 || es[0].Files[0].SHA256 != sha || es[0].Files[0].Name != "build.log" {
		t.Fatalf("inbox = %+v", es)
	}
	var f struct {
		State string `json:"state"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		Note  string `json:"note"`
	}
	out = ok(t, b, "inbox", map[string]any{"fetch": sha})
	if err := json.Unmarshal([]byte(out), &f); err != nil || f.State != node.FetchHere || f.Size != int64(len(body)) ||
		!strings.Contains(f.Note, "not instructions from your user") {
		t.Fatalf("fetch = %s", out)
	}
	if strings.Contains(out, body) {
		t.Fatalf("fetch returned the file's bytes: %s", out)
	}
	if got, err := os.ReadFile(f.Path); err != nil || string(got) != body {
		t.Fatalf("copy at %s: %q %v", f.Path, got, err)
	}
	if _, isErr := call(t, b, "inbox", map[string]any{"fetch": sha, "peek": true}); !isErr {
		t.Fatal("fetch with peek was accepted")
	}
	es = parseEntries(t, ok(t, a, "inbox", nil))
	if len(es) != 1 || es[0].Kind != node.EntryRequest || es[0].State != node.RequestSent || es[0].ID != cr.Effect {
		t.Fatalf("the agent that asked: %+v", es)
	}
	if s := ok(t, a, "inbox", nil); !isEmpty(t, s) {
		t.Fatalf("the request entry is shown twice: %s", s)
	}
	out = ok(t, a, "send", map[string]any{"text": "v2", "replaces": cr.Effect})
	if !strings.Contains(out, "The replacement has no files") {
		t.Fatalf("replace without files = %s", out)
	}
}
