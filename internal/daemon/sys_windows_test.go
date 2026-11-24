//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestWindowsNewHomeIsCreatedWithOnlyThisUserInItsAccessControlList(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if err := PrepareHome(home); err != nil {
		t.Fatal(err)
	}
	sddl, err := securityText(home)
	if err != nil {
		t.Fatal(err)
	}
	user, owner, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sddl, "D:P") || strings.Count(sddl, "(") != 1 || !strings.Contains(sddl, user) {
		t.Fatalf("home security descriptor %s", sddl)
	}
	// A file created inside inherits it.
	if err := os.WriteFile(filepath.Join(home, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fs, err := securityText(filepath.Join(home, "f")); err != nil || checkSDDL(fs, user, []string{user, owner}, "f") != nil {
		t.Fatalf("file inside: %s %v", fs, err)
	}
}

func TestWindowsHomeEveryoneCanOpenIsRefused(t *testing.T) {
	home := filepath.Join(t.TempDir(), "open")
	sddl, _ := syscall.UTF16PtrFromString("D:P(A;OICI;FA;;;WD)")
	var sd uintptr
	if r, _, e := procSDFromString.Call(uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		t.Fatal(e)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	p, _ := syscall.UTF16PtrFromString(home)
	sa := syscall.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := syscall.CreateDirectory(p, &sa); err != nil {
		t.Fatal(err)
	}
	if err := PrepareHome(home); err == nil || !strings.Contains(err.Error(), "accessible to other users") {
		t.Fatalf("PrepareHome: %v", err)
	}
}

func TestWindowsSecondLockOnTheHomeFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := tryLock(a); err != nil {
		t.Fatal(err)
	}
	if tryLock(b) == nil {
		t.Fatal("second lock taken")
	}
	unlock(a)
	if err := tryLock(b); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func TestWindowsSocketPathTooLongIsRefusedWithAReason(t *testing.T) {
	_, err := listenLocal(`C:\` + strings.Repeat("a", 120) + `\sock`)
	if err == nil || !strings.Contains(err.Error(), "A2P2_HOME") {
		t.Fatal(err)
	}
}
