package daemon

import (
	"os"
	"runtime"
)

// sockTemp is where tests make directories that hold a Unix socket: /tmp,
// whose short path fits the socket path limit on macOS (about 100 bytes),
// and on Windows, which has no /tmp, the user's temporary directory.
func sockTemp() string {
	if runtime.GOOS == "windows" {
		return os.TempDir()
	}
	return "/tmp"
}
