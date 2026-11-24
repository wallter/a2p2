package daemon

import (
	"context"
	"io"

	"github.com/wallter/a2p2/internal/node"
)

// filesPeerHandler is peerHandler with the file route (PROTOCOL 10.3).
type filesPeerHandler struct {
	peerHandler
	f *node.Files
}

func (h filesPeerHandler) ServeGet(ctx context.Context, from string, q node.FileGet) (*node.FileServe, error) {
	if !h.g.enter() {
		return nil, errStopping
	}
	defer h.g.leave()
	return h.f.ServeGet(ctx, from, q)
}

func (h filesPeerHandler) ServePut(ctx context.Context, from string, q node.FilePut, body io.Reader) (int64, error) {
	if !h.g.enter() {
		return 0, errStopping
	}
	defer h.g.leave()
	return h.f.ServePut(ctx, from, q, body)
}
