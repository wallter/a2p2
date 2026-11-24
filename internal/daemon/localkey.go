package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// The local key. On Windows a socket file's mode does not protect it, so
// besides the home's access control list (which the socket file inherits,
// and connecting needs write access to it) the daemon writes a fresh random
// key into the home at each start and refuses every local API request that
// does not present it. Only a process that can read the home can call the
// daemon. On Unix the key is not used (localKeyRequired is false) unless a
// test asks for it, so the boundary there is unchanged: the socket's mode
// and the home's permissions (DESIGN section 1).

const localKeyFile = "local.key"

func localKeyPath(home string) string { return filepath.Join(home, localKeyFile) }

// writeLocalKey makes a new key and writes it into home, readable by this
// user only (on Windows through the home's inherited access control list).
func writeLocalKey(home string) (string, error) {
	key, err := newToken()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(localKeyPath(home), []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	return key, nil
}

// readLocalKey returns the key the running daemon wrote, or "" if there is
// none (Unix, or no daemon).
func readLocalKey(home string) string {
	b, err := os.ReadFile(localKeyPath(home))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// errNoKey is what a caller without the key is told; nothing else.
var errNoKey = errors.New("this a2p2 daemon needs the local key from its home directory")

// requireKey serves h only for requests that present key.
func requireKey(key string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(localapi.KeyHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(localapi.Refusal{Error: errNoKey.Error(), Code: node.ErrorNotAllowed})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Client is the local API client for home: it presents the key when the
// daemon wrote one, read again at each call so that a daemon started after
// the client was made is still reached.
func Client(home string) *localapi.Client {
	return localapi.NewClient(sockPath(home)).WithKey(func() string { return readLocalKey(home) })
}
