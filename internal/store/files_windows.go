//go:build windows

package store

import "os"

// syncDir is a no-op on Windows, and this is what a2p2 relies on there
// instead: Go cannot open a directory for FlushFileBuffers, so a rename,
// a link or a new directory is durable only as far as NTFS's metadata
// journal makes it (it commits the change within seconds, not at the
// call). Each file's bytes are synced before it is renamed or linked, as
// on every system, so a crash can lose a publication but never expose
// short bytes; what a crash loses is recovered at the start: a row whose
// file is missing is reset (reconcileParts), a blob without a row is
// removed, and a deletion stays queued until it succeeds.
func syncDir(string) error { return nil }

// openSource opens a path a person named for reading, once. Modes mean
// nothing on Windows (D47): files a2p2 writes inherit the home's access
// list. The caller refuses anything but a regular file from the handle.
func openSource(path string) (*os.File, error) { return os.Open(path) }
