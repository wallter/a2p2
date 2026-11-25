//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// syncDir is a no-op on Windows: Go cannot open a directory for
// FlushFileBuffers, so a rename, a link, a removal or a new directory is
// durable only as far as NTFS's metadata journal makes it (it commits the
// change within seconds, not at the call). Each file's bytes are synced
// before it is renamed or linked, as on every system, so a crash can lose
// a publication or undo a removal but never expose short bytes. What a2p2
// does about it at the start (ReconcileFiles): a held blob or a written
// copy whose file is gone loses its row, a file under blobs/ without a row
// is removed, every part is dropped, and a removal stays queued
// (dirSyncDurable) until a start sees its path gone.
func syncDir(string) error { return nil }

// dirSyncDurable: a removal here is not known durable until a start.
const dirSyncDurable = false

// oNoFollow: Windows has no such open flag; the caller looks with Lstat
// first and refuses anything but a regular file.
const oNoFollow = 0

// fileIdent is the identity of an open file: its volume and file index.
func fileIdent(f *os.File) (string, error) {
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x:%x", d.VolumeSerialNumber, d.FileIndexHigh, d.FileIndexLow,
		uint64(d.LastWriteTime.HighDateTime)<<32|uint64(d.LastWriteTime.LowDateTime)), nil
}

// openSource opens a path a person named for reading, once. Modes mean
// nothing on Windows (D47): files a2p2 writes inherit the home's access
// list. The caller refuses anything but a regular file from the handle.
func openSource(path string) (*os.File, error) { return os.Open(path) }

// remoteOrDevice: a UNC path, a \\?\ or \\.\ namespace, or a reserved
// device name in any directory; refused before any I/O [fix-files-b 7].
func remoteOrDevice(path string) bool {
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`) || strings.HasPrefix(path, `\/`) || strings.HasPrefix(path, `/\`) {
		return true
	}
	stem := strings.ToUpper(strings.SplitN(filepath.Base(path), ".", 2)[0])
	return windowsReserved[strings.TrimRight(stem, " ")]
}

var getFinalPathNameByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")

// handlePath is the path the system reports for the open file f.
func handlePath(f *os.File) (string, error) {
	buf := make([]uint16, 32768)
	n, _, err := getFinalPathNameByHandle.Call(f.Fd(), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if n == 0 || n >= uintptr(len(buf)) {
		return "", err
	}
	p := syscall.UTF16ToString(buf[:n])
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(p, `\\?\`), nil
}
