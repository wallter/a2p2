//go:build windows

package store

import "testing"

// TestRemoteAndDevicePathsAreRefusedByNameBeforeAnyIO (fix-files-b 7):
// UNC paths, the \\?\ and \\.\ namespaces and reserved device names in
// any directory are refused from the name alone.
// Requirements: L1, P3
func TestRemoteAndDevicePathsAreRefusedByNameBeforeAnyIO(t *testing.T) {
	for p, want := range map[string]bool{
		`\\server\share\a.txt`: true, `\\?\C:\a.txt`: true, `\\.\PhysicalDrive0`: true, `//server/share/a.txt`: true,
		`C:\tmp\NUL`: true, `C:\tmp\con.txt`: true, `C:\tmp\COM1 .log`: true,
		`\??\UNC\evil.example\s\a.txt`: true, `C:\tmp\a.txt:s`: true, `C:\tmp\NUL:`: true, `C:\tmp\COM0`: true,
		`C:\tmp\a.txt`: false, `C:\tmp\console.txt`: false,
	} {
		if got := RemoteOrDevice(p); got != want {
			t.Errorf("RemoteOrDevice(%q) = %v, want %v", p, got, want)
		}
	}
}
