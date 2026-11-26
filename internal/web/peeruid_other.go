//go:build !linux

package web

import "net/http"

// peerUID is not known here: the code alone decides (peeruid.go).
func peerUID(*http.Request) (int, bool) { return 0, false }
