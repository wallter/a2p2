//go:build windows

package store

import (
	"fmt"
	"os"
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
var dirSyncDurable = false

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

// oNonBlock: an open does not wait on Windows.
const oNonBlock = 0

var getDriveType = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")

// driveRemote reports whether root (such as Z:\) is a drive letter mapped
// to a network share (DRIVE_REMOTE). It reads the system's table of drive
// letters. Unverified on Windows: written from the documentation of
// GetDriveTypeW.
func driveRemote(root string) bool {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return false
	}
	const driveRemoteType = 4 // DRIVE_REMOTE
	t, _, _ := getDriveType.Call(uintptr(unsafe.Pointer(p)))
	return t == driveRemoteType
}

// remoteOrDevice is windowsRemoteOrDevice (files_source.go, where tests on
// every system run it).
func remoteOrDevice(path string) bool { return windowsRemoteOrDevice(path) }

// linkCount is the number of names the open file f has.
func linkCount(f *os.File) int {
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return 0
	}
	return int(d.NumberOfLinks)
}

// linkTarget is a link's target as read, with the NT prefix of a local
// drive removed (\??\C:\x is C:\x); any other form stays as it is, and
// remoteOrDevice refuses it.
func linkTarget(t string) string {
	if rest, ok := strings.CutPrefix(t, `\??\`); ok && len(rest) >= 3 && rest[1] == ':' && rest[2] == '\\' {
		return rest
	}
	return t
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
