package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// The daemon's log. A daemon that Connect starts writes to home/daemon.log
// through the handle it inherits, opened for appending. Another machine can
// make the daemon log (a refused request is a line), so the running daemon,
// which holds the home's lock, is the one owner of the file's size: every
// logEvery, once the file is larger than logMax, its last logMax bytes are
// copied to daemon.log.1, which replaces the one before, and the file is
// cut to nothing. The writer appends, so its next line starts the file
// again. If the copy cannot be made (a full disk, say) the file is cut all
// the same: losing the log is better than filling the disk. So the two
// files hold at most logMax each, plus what is written between two checks.
// Lines written during the copy may be lost.

const (
	logMax   = 10 << 20         // bytes
	logEvery = 10 * time.Second // how often a running daemon checks
)

func logPath(home string) string { return filepath.Join(home, "daemon.log") }

// rotateLog moves the last max bytes of the log at path to path+".1", and
// empties the log, if it is larger than max. An error making path+".1" is
// returned after the log is emptied anyway.
func rotateLog(path string, max int64) error {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= max || !fi.Mode().IsRegular() {
		if os.IsNotExist(err) {
			err = nil
		}
		return err
	}
	err = archiveLog(path, fi.Size(), max)
	if terr := os.Truncate(path, 0); terr != nil {
		return terr
	}
	return err
}

// archiveLog writes the last max of the first size bytes of path to
// path+".1", through a temporary file in the same directory.
func archiveLog(path string, size, max int64) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := src.Seek(size-max, io.SeekStart); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp") // 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, io.LimitReader(src, max))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path+".1")
}

// keepLog checks the log at path every interval until ctx ends, and once
// at the start.
func keepLog(ctx context.Context, path string, max int64, every time.Duration) {
	warned := false
	for {
		if err := rotateLog(path, max); err != nil && !warned {
			warned = true
			slog.Warn("the daemon log could not be rotated", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
