package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// rawCall posts body to /v1/name and returns the status and the decoded
// response object, so the test sees the wire, not the client's reading.
func rawCall(t *testing.T, c *Client, name, body string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://a2p2/v1/"+name, strings.NewReader(body))
	res, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: not a JSON object: %s", name, raw)
	}
	return res.StatusCode, m, string(raw)
}

// C2: every refusal is {"error","code","retryable"}: the sentence as before,
// a code from the one list, and whether repeating the call can help. The
// client keeps the sentence as the error text, so the CLI prints it.
func TestEveryRefusalCarriesAStableCodeAndWhetherRetryingCanHelp(t *testing.T) {
	a, _ := newPair(t)
	ctx := context.Background()
	alice := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "alice", Cwd: "/w/alice"}).ID
	bob := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "bob", Cwd: "/w/bob"}).ID
	direct := call[SendResp](t, a.c, "send", SendReq{Agent: alice, Text: "for bob", To: []string{bob}}).ID
	plain := call[SendResp](t, a.c, "send", SendReq{Agent: alice, Text: "for all"}).ID
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	for _, x := range []struct {
		name, body, code string
		retry            bool
		says             string
	}{
		{"send", `{"agent":`, node.ErrorInvalid, false, "bad request"},
		{"send", j(SendReq{Agent: alice, Text: "x", Room: "nowhere"}), node.ErrorNotFound, false, `no room "nowhere" here`},
		{"ack", j(AckReq{Agent: bob, ID: strings.Repeat("0", 32)}), node.ErrorNotFound, false, "no message"},
		{"ack", j(AckReq{Agent: bob, ID: plain}), node.ErrorNotAllowed, false, "not addressed to you"},
		{"ack", j(AckReq{Agent: alice, ID: direct}), node.ErrorNotAllowed, false, "you wrote this message"},
		{"ack", j(AckReq{Agent: bob, ID: direct}), node.ErrorNotShown, false, "read it with inbox first"},
		{"inbox", j(InboxReq{Agent: "ghost@" + a.id[:8]}), node.ErrorNotRegistered, true, "not registered"},
		{"send", j(SendReq{Agent: alice, Text: strings.Repeat("x", 20000)}), node.ErrorTooLarge, false, "too long"},
		{"room/invite", j(InviteReq{Room: "local"}), node.ErrorNotAllowed, false, "local room"},
		{"room/leave", j(RoomReq{Room: "local"}), node.ErrorNotAllowed, false, "cannot be left"},
		{"confirm", j(ConfirmReq{ID: "q_nope", Approve: true}), node.ErrorNotFound, false, "no such request"},
	} {
		status, m, raw := rawCall(t, a.c, x.name, x.body)
		if status != http.StatusBadRequest || len(m) != 3 || m["code"] != x.code || m["retryable"] != x.retry ||
			!strings.Contains(m["error"].(string), x.says) {
			t.Errorf("%s %s: %d %s, want code %s retryable %v saying %q", x.name, x.body, status, raw, x.code, x.retry, x.says)
		}
		err := a.c.Call(ctx, x.name, json.RawMessage(x.body), nil)
		var e *Error
		if x.body != `{"agent":` && (!asError(err, &e) || e.Code != x.code || e.Retryable != x.retry || e.Error() != m["error"]) {
			t.Errorf("%s: client error %#v", x.name, err)
		}
	}
	// Every code is in the one list with its retry rule, and PROTOCOL
	// section 7 lists each one.
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	sec := s[strings.Index(s, "## 7. Local API"):strings.Index(s, "## 8. ")]
	for _, c := range node.ErrorCodes {
		if !strings.Contains(sec, "`"+c+"`") {
			t.Errorf("PROTOCOL section 7 does not list code %s", c)
		}
	}
	// An unreachable daemon is unavailable and retryable.
	r := RefusalOf(NewClient("/nonexistent.sock").Call(ctx, "status", nil, nil))
	if r.Code != node.ErrorUnavailable || !r.Retryable || r.Error == "" {
		t.Errorf("unreachable: %+v", r)
	}
}

func asError(err error, e **Error) bool {
	x, ok := err.(*Error)
	*e = x
	return ok
}

// Item 4: text is written as it is. '<', '>' and '&' are not escaped in a
// response (the CLI's --json prints it unchanged), and still quoted as
// JSON, so it cannot forge structure.
func TestResponsesWriteAngleBracketsAndAmpersandsAsWritten(t *testing.T) {
	a, _ := newPair(t)
	alice := call[store.Agent](t, a.c, "register", RegisterReq{Harness: "test", Label: "alice", Cwd: "/w/alice"}).ID
	text := `if a < b && c > d { "<script>" }`
	call[SendResp](t, a.c, "send", SendReq{Agent: alice, Text: text})
	raw, err := a.c.CallRaw(context.Background(), "history", HistoryReq{Room: "local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	escaped := []byte(`\` + "u00") // how encoding/json escapes them by default
	if !bytes.Contains(raw, []byte(`if a < b && c > d { \"<script>\" }`)) || bytes.Contains(raw, escaped) {
		t.Fatalf("history = %s", raw)
	}
}
