// Package peer is the node-to-node layer: the node's key, and mutual-TLS
// HTTP between nodes. Nothing here decides what a peer may do; it only proves
// which node is on the other end and hands the request to internal/node.
package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Identity is a node's long-lived Ed25519 key. The node id is the public key.
type Identity struct {
	Key ed25519.PrivateKey
}

// NodeID is the lowercase hex public key (64 characters).
func (i *Identity) NodeID() string {
	return hex.EncodeToString(i.Key.Public().(ed25519.PublicKey))
}

// LoadIdentity reads home/identity.key, creating it on first use. The file
// holds the 32-byte seed in hex and must be readable only by its owner.
func LoadIdentity(home string) (*Identity, error) {
	path := filepath.Join(home, "identity.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		// O_EXCL: never overwrite a key that appeared in the meantime.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(hex.EncodeToString(seed) + "\n"); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return &Identity{Key: ed25519.NewKeyFromSeed(seed)}, nil
	}
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// Windows has no file modes (Go reports 0666); the key is guarded by
	// the home's access control list, which the daemon checks.
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users (mode %o); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	seed, err := hex.DecodeString(string(trimSpace(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a valid a2p2 key", path)
	}
	return &Identity{Key: ed25519.NewKeyFromSeed(seed)}, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
