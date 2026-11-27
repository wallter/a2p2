package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// The databases in testdata/releases/<tag>/ were written by that tagged
// release's own code (scripts/release-db.sh runs
// testdata/releases/gen_test.go.txt in a copy of the tag's source): three
// machines of one room with a reply, a question with choices and a handled
// report, an edit, a withdrawal, an addressed message, a removal, an agent
// that read and confirmed, and a second room of one machine.

type compatRoom struct {
	Label   string   `json:"label"`
	Records int      `json:"records"`
	Members []string `json:"members"`
	Admins  []string `json:"admins"`
}

type compatMachine struct {
	ID     string                `json:"id"`
	Agents []string              `json:"agents"`
	Rooms  map[string]compatRoom `json:"rooms"`
}

// compatReleases are the tagged releases whose databases must be in
// testdata/releases. A release is added here when its databases are
// committed (docs/RELEASING.md); a missing one fails the test.
var compatReleases = []string{"v0.2.0", "v0.2.1", "v0.2.2", "v0.2.3"}

var compatUserVersion = regexp.MustCompile(`(?m)^PRAGMA user_version = ([0-9]+);$`)

// compatVersion is the schema version a release wrote, read from the last
// line of its SQL text.
func compatVersion(t *testing.T, b []byte) int {
	t.Helper()
	m := compatUserVersion.FindSubmatch(b)
	if m == nil {
		t.Fatal("the SQL text has no user_version")
	}
	v, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// compatDBVersion reads user_version of the database file at path.
func compatDBVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", store.FileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// openReleaseMachine loads dir/name.sql into a fresh file and opens it as
// the machine name with the current code, as newLNode would. It returns
// the schema version the release wrote.
func openReleaseMachine(t *testing.T, dir, name string, clock *time.Time) (*lnode, int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	was := compatVersion(t, b)
	path := filepath.Join(t.TempDir(), "a2p2.db")
	db, err := sql.Open("sqlite", store.FileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(b)); err != nil {
		db.Close()
		t.Fatalf("%s/%s.sql: %v", dir, name, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("rooms test " + name))
	key := ed25519.NewKeyFromSeed(seed[:])
	n := &lnode{t: t, name: name, key: key, id: hex.EncodeToString(key.Public().(ed25519.PublicKey)),
		path: path, clock: clock,
		// The test's random bytes are a counter; start it past every value
		// the release's run used, as real random bytes would be, so a new
		// stream does not get an epoch that run already gave out.
		n: 1 << 40}
	n.open()
	t.Cleanup(func() { n.st.Close() })
	return n, was
}

// compatTexts is the text of each message entry in v, by author agent
// label.
func compatTexts(v InboxView) map[string][]string {
	out := map[string][]string{}
	for _, e := range v.Entries {
		if e.Kind == "message" {
			label := e.From
			if i := strings.IndexByte(label, '@'); i >= 0 {
				label = label[:i]
			}
			out[label] = append(out[label], e.Text)
		}
	}
	return out
}

// TestDatabasesFromEachReleaseOpenReplayAndKeepWorking opens the database
// of each machine as each tagged release left it, with the current code:
// the database opens at a schema version no lower than the release wrote
// (any later migrations run), every room log replays, every record is
// still held, membership is what that release worked out, the inboxes
// show what that release left unread (alice the messages of bob and
// carol; bob, who confirmed everything, nothing), and a and b can still
// send to each other and pull. A new rule that refuses old entries or
// records, or a migration that loses data, fails here (F71, F72, F73 in
// the 2026-11-25 review). Releases v0.2.0 to v0.2.3 all wrote schema 14,
// the current one, so no migration runs yet; the first release with a new
// migration exercises one. Old and new code on two machines talking over
// the network is not tested here.
//
// Requirements: I5
func TestDatabasesFromEachReleaseOpenReplayAndKeepWorking(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "releases", "v*"))
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, d := range dirs {
		have = append(have, filepath.Base(d))
	}
	for _, v := range compatReleases {
		if !slices.Contains(have, v) {
			t.Errorf("no databases for %s in testdata/releases (scripts/release-db.sh %s)", v, v)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	ctx := context.Background()
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			j, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]compatMachine
			if err := json.Unmarshal(j, &manifest); err != nil {
				t.Fatal(err)
			}
			// An hour after the release's run: later than every record in it.
			c := lclock().Add(time.Hour)
			clock := &c
			net := newMemNet()
			nodes := map[string]*pnode{}
			for _, name := range []string{"a", "b", "c"} {
				want, ok := manifest[name]
				if !ok {
					t.Fatalf("manifest has no machine %s", name)
				}
				n, was := openReleaseMachine(t, dir, name, clock)
				if now := compatDBVersion(t, n.path); now < was {
					t.Fatalf("%s: schema version %d after the open, the release wrote %d", name, now, was)
				}
				if n.id != want.ID {
					t.Fatalf("%s: key %s, the manifest says %s", name, n.id, want.ID)
				}
				// A room whose log no longer loads is set aside rather than
				// failing the open (D57): here that is a failure.
				if q := n.r.Quarantined(); len(q) != 0 {
					t.Fatalf("%s: rooms set aside: %+v", name, q)
				}
				var held []string
				for r := range n.r.rooms {
					held = append(held, r)
				}
				slices.Sort(held)
				var wantRooms []string
				for r := range want.Rooms {
					wantRooms = append(wantRooms, r)
				}
				slices.Sort(wantRooms)
				if !slices.Equal(held, wantRooms) {
					t.Fatalf("%s holds rooms %v, want %v", name, held, wantRooms)
				}
				for r, wr := range want.Rooms {
					if got := len(n.records(r)); got != wr.Records {
						t.Errorf("%s %s: %d records, want %d", name, wr.Label, got, wr.Records)
					}
					st := n.state(r)
					compatSame(t, name+" "+wr.Label+" members", compatSorted(st.Members), wr.Members)
					compatSame(t, name+" "+wr.Label+" admins", compatSorted(st.Admins), wr.Admins)
					for _, agent := range want.Agents {
						iv, err := n.m.Inbox(ctx, r, agent)
						if err != nil {
							t.Errorf("%s %s: inbox of %s: %v", name, wr.Label, agent, err)
							continue
						}
						if wr.Label != "team" {
							continue
						}
						got := compatTexts(iv)
						switch agent {
						case "alice":
							// Never read: bob's reply and carol's message.
							if !slices.Contains(got["bob"], "a reply") || !slices.Contains(got["carol"], "from c") {
								t.Errorf("a team: alice's inbox lost unread messages: %v", got)
							}
						case "bob":
							// bob confirmed every entry his inbox showed.
							if len(iv.Entries) != 0 {
								t.Errorf("b team: bob confirmed everything, but his inbox shows %d entries: %v", len(iv.Entries), got)
							}
						}
					}
				}
				nodes[name] = newPNode(t, net, name, n)
			}
			// a and b are still members of team: a new message from a
			// reaches b.
			a, b := nodes["a"], nodes["b"]
			var team string
			for r, wr := range manifest["a"].Rooms {
				if wr.Label == "team" {
					team = r
				}
			}
			if team == "" {
				t.Fatal("manifest has no team room for a")
			}
			before := len(b.records(team))
			id := a.send(team, "alice", "sent by the current code")
			b.mustPull(team, a, PullOpts{})
			if got := len(b.records(team)); got != before+1 {
				t.Fatalf("b holds %d records of team after the pull, want %d", got, before+1)
			}
			found := false
			for _, r := range b.records(team) {
				if r.ID() == id && r.Fields().Kind == core.KindMessage {
					found = true
				}
			}
			if !found {
				t.Fatal("b does not hold the new message")
			}
			// and a reply from b reaches a.
			before = len(a.records(team))
			back := b.send(team, "bob", "sent back by the current code")
			a.mustPull(team, b, PullOpts{})
			if got := len(a.records(team)); got != before+1 {
				t.Fatalf("a holds %d records of team after the pull, want %d", got, before+1)
			}
			iv, err := a.m.Inbox(ctx, team, "alice")
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, e := range iv.Entries {
				if e.ID == back && e.Text == "sent back by the current code" {
					seen = true
				}
			}
			if !seen {
				t.Fatal("alice's inbox does not show the message b sent")
			}
		})
	}
}

func compatSorted(xs []string) []string {
	ys := slices.Clone(xs)
	slices.Sort(ys)
	if ys == nil {
		ys = []string{}
	}
	return ys
}

func compatSame(t *testing.T, what string, got, want []string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}
