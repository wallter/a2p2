//go:build windows

package harness

import "os"

// fileOwner is not read on Windows; checkBinary checks only the path there.
func fileOwner(os.FileInfo) (int, bool) { return 0, false }
