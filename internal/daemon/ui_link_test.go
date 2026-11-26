package daemon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// pageState is GET /api/state of the page at host with session.
func pageState(t *testing.T, host, session string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+host+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// F14: the address status gives (and a2p2 ui opens, so xdg-open and the
// browser carry it in their arguments) holds only a one-time code: each
// status gives a new one, and the code is no session. A session lasts the
// daemon process, across a networking decision applied in place, and
// ends with the process.
func TestThePagesAddressCarriesOnlyAOneTimeCode(t *testing.T) {
	m := newMachineWith(t, "a", nil)
	links := map[string]bool{}
	for i := 0; i < 3; i++ {
		st := call[localapi.Status](m, "status", localapi.Empty{})
		u, err := url.Parse(st.UI)
		if err != nil || !strings.HasPrefix(u.Fragment, "c=") || u.Scheme != "http" {
			t.Fatalf("status ui %q", st.UI)
		}
		if pageState(t, u.Host, strings.TrimPrefix(u.Fragment, "c=")) != 401 || pageState(t, u.Host, u.Fragment) != 401 {
			t.Fatal("the address's code works as a session")
		}
		links[st.UI] = true
	}
	if len(links) != 3 {
		t.Fatalf("status gave one address twice: %v", links)
	}
	host, session := m.uiSession()
	if pageState(t, host, session) != 200 {
		t.Fatal("the session is refused")
	}
	m.setNet(node.NetworkOn) // a run started again in place
	if pageState(t, host, session) != 200 {
		t.Fatal("a networking decision signed the page out")
	}
	m.stop()
	m.start()
	host2, _ := m.uiSession()
	if pageState(t, host2, session) != 401 {
		t.Fatal("a session outlived its daemon process")
	}
}

// F47: when the page's address is taken (another local user may be
// listening there), the daemon serves on another port, says so in its
// log, and status gives the port it serves on.
func TestATakenPageAddressIsSaidInTheLog(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	logs := captureLog(t)
	m := newMachineWith(t, "a", func(home string) {
		if err := SaveConfig(home, Config{Name: "a", UI: taken.Addr().String()}); err != nil {
			t.Fatal(err)
		}
	})
	host, session := m.uiSession()
	if host == taken.Addr().String() || pageState(t, host, session) != 200 {
		t.Fatalf("the page is on %s, the taken address is %s", host, taken.Addr())
	}
	if !strings.Contains(logs.String(), "address is taken") || !strings.Contains(logs.String(), taken.Addr().String()) {
		t.Fatalf("the log does not say the address was taken:\n%s", logs)
	}
}

// F19: a running daemon keeps its log to about logMax: a log already
// larger at the start is moved to daemon.log.1, its last logMax bytes.
func TestARunningDaemonRotatesItsLog(t *testing.T) {
	m := newMachineWith(t, "a", func(home string) {
		if err := os.WriteFile(logPath(home), []byte(strings.Repeat("x", logMax+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	// The archive is renamed into place before the log is cut, so wait for both.
	eventually(t, "the log to be rotated", func() bool {
		fi, err := os.Stat(logPath(m.home) + ".1")
		cur, cerr := os.Stat(logPath(m.home))
		return err == nil && fi.Size() == logMax && cerr == nil && cur.Size() < 1<<20
	})
}

// F14: the page's server bounds how long a request may take to arrive,
// body included, so a local program cannot hold a connection open by
// sending its body slowly (a trade of a code, or any other POST).
func TestThePageClosesARequestWhoseBodyIsSentSlowly(t *testing.T) {
	prev := uiReadTimeout
	uiReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { uiReadTimeout = prev })
	m := newMachineWith(t, "a", nil)
	host, _ := m.uiSession()
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "POST /api/session HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\n"+
		"Content-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", host, host); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	_, err = io.Copy(io.Discard, c) // returns when the server closes the connection
	if err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("the connection was held open: %v after %v", err, time.Since(start))
	}
}
