package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The page's sign-in (PROTOCOL 7). No long-lived secret is put in a URL or
// on a command line: `a2p2 ui` gets a link with a one-time code, which a
// browser started by xdg-open keeps in its process arguments, where other
// users of the machine can read it. The page trades the code for a
// session (POST /api/session, the page's exact Origin) and keeps that in
// the tab's sessionStorage. A code works once and for CodeLife. A code
// used a second time ends the session the first use made, since one of
// the two uses was not the person's. A traded code is remembered for as
// long as its session lives, apart from the untraded codes, so this holds
// after the code's CodeLife has passed and after newer codes have pushed
// it out of the untraded ones.

const (
	// CodeLife is how long a link's code can be traded for a session.
	CodeLife = 30 * time.Second
	// maxCodes and maxSessions bound what a process keeps; past them the
	// oldest goes first. Every status call makes one code (the CLI's
	// connect, the MCP shim and hooks call it too), so maxCodes is set far
	// above what a busy machine makes in CodeLife, so that a code printed
	// with --print is not pushed out before it is used. A traded code is
	// kept with its session, so traded codes are bounded by maxSessions.
	maxCodes    = 1024
	maxSessions = 32
)

// Sessions are the page's sessions and the codes not yet traded, for one
// daemon process: they last while it runs and are never stored, so a
// restart signs every tab out. Each session has its own run, a random
// value given with the session and carried by each state the session
// reads; the page refuses a state with another (a tab left open on a port
// that another local user took after this daemon stopped). It is not
// derived from the token, which a listener on the port receives, and no
// other session's holder learns it, so a user who traded a code of their
// own, or the person's code first, cannot answer as the person's session.
type Sessions struct {
	mu       sync.Mutex
	now      func() time.Time
	codes    map[string]time.Time // untraded codes by hash: when each expires
	codeAge  []string             // untraded code hashes, oldest first
	traded   map[string]string    // traded code hash: the hash of the session it gave
	sessions map[string]session   // by session token hash
	sessAge  []string             // session token hashes, oldest first
}

// session is what the server keeps of one: the hash of the code that gave
// it (or "") and its run.
type session struct{ code, run string }

// NewSessions returns an empty set.
func NewSessions() *Sessions {
	return &Sessions{now: time.Now, codes: map[string]time.Time{},
		traded: map[string]string{}, sessions: map[string]session{}}
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return hex.EncodeToString(b)
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Link returns the page's address on addr with a new one-time code.
func (ss *Sessions) Link(addr string) string { return "http://" + addr + "/#c=" + ss.Code() }

// Code makes a new one-time code.
func (ss *Sessions) Code() string {
	c := random(16)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.prune()
	for len(ss.codeAge) >= maxCodes {
		delete(ss.codes, ss.codeAge[0])
		ss.codeAge = ss.codeAge[1:]
	}
	h := hashOf(c)
	ss.codes[h] = ss.now().Add(CodeLife)
	ss.codeAge = append(ss.codeAge, h)
	return c
}

// prune forgets expired codes. ss.mu is held.
func (ss *Sessions) prune() {
	now := ss.now()
	for len(ss.codeAge) > 0 {
		if exp, ok := ss.codes[ss.codeAge[0]]; ok && now.Before(exp) {
			return
		}
		delete(ss.codes, ss.codeAge[0])
		ss.codeAge = ss.codeAge[1:]
	}
}

// Trade outcomes besides success.
const (
	tradeUnknown = iota + 1 // no such code, or it expired untraded
	tradeReused             // used before: the session it gave is ended
)

// trade turns code into a new session token and its run, once.
func (ss *Sessions) trade(code string) (tok, run string, why int) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.prune()
	ch := hashOf(code)
	if sh, ok := ss.traded[ch]; ok {
		ss.end(sh)
		return "", "", tradeReused
	}
	if _, ok := ss.codes[ch]; !ok {
		return "", "", tradeUnknown
	}
	delete(ss.codes, ch)
	for i, x := range ss.codeAge {
		if x == ch {
			ss.codeAge = append(ss.codeAge[:i], ss.codeAge[i+1:]...)
			break
		}
	}
	tok, run = random(24), random(16)
	sh := hashOf(tok)
	ss.add(sh, session{code: ch, run: run})
	ss.traded[ch] = sh
	return tok, run, 0
}

// add keeps a session by its token hash, ending the oldest past
// maxSessions. ss.mu is held.
func (ss *Sessions) add(h string, s session) {
	for len(ss.sessAge) >= maxSessions {
		ss.end(ss.sessAge[0])
	}
	ss.sessions[h] = s
	ss.sessAge = append(ss.sessAge, h)
}

// end forgets a session token hash and the code that gave it: once the
// session is gone, a later use of the code has nothing to end and is
// refused as unknown. ss.mu is held.
func (ss *Sessions) end(h string) {
	if s, ok := ss.sessions[h]; ok && s.code != "" {
		delete(ss.traded, s.code)
	}
	delete(ss.sessions, h)
	for i, x := range ss.sessAge {
		if x == h {
			ss.sessAge = append(ss.sessAge[:i], ss.sessAge[i+1:]...)
			break
		}
	}
}

// valid reports whether tok is a session of this process. Tokens are
// looked up by their hash, so the lookup's timing says nothing useful
// about a token.
func (ss *Sessions) valid(tok string) bool {
	_, ok := ss.runOf(tok)
	return ok
}

// runOf returns the run of the session tok, if it is one.
func (ss *Sessions) runOf(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.sessions[hashOf(tok)]
	return s.run, ok
}

// session trades a link's code for a session. It needs the page's exact
// Origin, as a decision does, so that no other site the browser opens can
// trade a code it learned; the code is the secret, so no token is asked.
func (sv *server) session(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "http://"+r.Host {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a link must be opened by the page itself (its exact Origin)"})
		return
	}
	if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
		writeJSON(w, 415, map[string]string{"error": "content-type must be application/json"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	// Where the system says who is at the other end (Linux), another user
	// is refused before the code is looked at, so the code stays good for
	// the person's browser and the attempt ends nothing. Root is not
	// refused: it can read the home anyway. Where the user is not known,
	// the code alone decides (peeruid.go).
	if uid, ok := peerOf(r); ok && uid != os.Getuid() && uid != 0 {
		slog.Warn("another user of this computer tried to open the page with a link; refused", "uid", uid)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this link was opened by another user of this computer; " +
			"only the user that runs this a2p2 daemon can open its page"})
		return
	}
	tok, run, why := sv.ss.trade(in.Code)
	switch why {
	case tradeUnknown:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this link has expired or was never valid; run `a2p2 ui` for a new one (a link works once, within " + CodeLife.String() + ")"})
		return
	case tradeReused:
		slog.Warn("a page link was used twice; the session its first use opened is ended")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this link was used already, so the page that used it is signed out; run `a2p2 ui` for a new one. " +
			"If you did not open it twice, another program or user may have read the link"})
		return
	}
	writeJSON(w, 200, map[string]string{"session": tok, "run": run})
}
