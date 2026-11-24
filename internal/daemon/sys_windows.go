//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

// On Windows the local boundary is the home's access control list, which
// lets only this user in (the socket file inherits it, and connecting to
// an AF_UNIX socket needs write access to its file), and the local key that
// every local API request must present (localkey.go). Raw calls to
// kernel32 and advapi32 keep golang.org/x/sys out of the direct
// dependencies (DECISIONS, D-windows).

const localKeyRequired = true

var (
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procSDFromString         = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procSDToString           = advapi32.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	procGetNamedSecurityInfo = advapi32.NewProc("GetNamedSecurityInfoW")
	procLockFileEx           = kernel32.NewProc("LockFileEx")
	procUnlockFileEx         = kernel32.NewProc("UnlockFileEx")
)

const (
	sddlRevision1            = 1
	seFileObject             = 1
	ownerSecurityInformation = 0x1
	daclSecurityInformation  = 0x4
	lockfileFailImmediately  = 0x1
	lockfileExclusiveLock    = 0x2
	detachedProcess          = 0x00000008
	createBreakawayFromJob   = 0x01000000
	unixPathMax              = 108 // afunix.h
)

// currentUser returns this process's user SID and the SID that owns what
// it creates by default (the user, or Administrators for an elevated
// administrator on some systems).
func currentUser() (user, owner string, err error) {
	t, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", "", err
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	if err != nil {
		return "", "", err
	}
	if user, err = u.User.Sid.String(); err != nil {
		return "", "", err
	}
	var n uint32
	_ = syscall.GetTokenInformation(t, syscall.TokenOwner, nil, 0, &n)
	if n >= uint32(unsafe.Sizeof(uintptr(0))) {
		b := make([]byte, n)
		if syscall.GetTokenInformation(t, syscall.TokenOwner, &b[0], n, &n) == nil {
			// TOKEN_OWNER is one pointer to a SID inside the same buffer.
			sid := *(**syscall.SID)(unsafe.Pointer(&b[0]))
			owner, _ = sid.String()
		}
	}
	return user, owner, nil
}

// makeHome creates the parents normally and the home itself with a
// protected access control list that lets only this user in.
func makeHome(home string) error {
	if fi, err := os.Lstat(home); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory (symlinks are not followed)", home)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(home), 0o700); err != nil {
		return err
	}
	user, _, err := currentUser()
	if err != nil {
		return err
	}
	sddl, err := syscall.UTF16PtrFromString(homeSDDL(user))
	if err != nil {
		return err
	}
	var sd uintptr
	if r, _, e := procSDFromString.Call(uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return fmt.Errorf("security descriptor: %w", e)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	p, err := syscall.UTF16PtrFromString(home)
	if err != nil {
		return err
	}
	sa := syscall.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := syscall.CreateDirectory(p, &sa); err != nil && !errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
		return &os.PathError{Op: "mkdir", Path: home, Err: err}
	}
	return nil
}

// checkPrivate refuses a home owned by someone else or whose access
// control list lets another user in.
func checkPrivate(_ os.FileInfo, home string) error {
	user, owner, err := currentUser()
	if err != nil {
		return err
	}
	sddl, err := securityText(home)
	if err != nil {
		return fmt.Errorf("%s: cannot read its access control list: %w", home, err)
	}
	return checkSDDL(sddl, user, []string{user, owner}, home)
}

// securityText returns the owner and DACL of path as SDDL.
func securityText(path string) (string, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var owner, dacl, sd uintptr
	r, _, _ := procGetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(p)), seFileObject,
		ownerSecurityInformation|daclSecurityInformation,
		uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	if r != 0 {
		return "", syscall.Errno(r)
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	var s *uint16
	var n uint32
	if r, _, e := procSDToString.Call(sd, sddlRevision1, ownerSecurityInformation|daclSecurityInformation,
		uintptr(unsafe.Pointer(&s)), uintptr(unsafe.Pointer(&n))); r == 0 {
		return "", e
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(s)))
	return syscall.UTF16ToString(unsafe.Slice(s, n)), nil
}

// tryLock takes an exclusive lock on the first byte of f without waiting.
func tryLock(f *os.File) error {
	var ov syscall.Overlapped
	r, _, e := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if r == 0 {
		return e
	}
	return nil
}

func unlock(f *os.File) {
	var ov syscall.Overlapped
	_, _, _ = procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
}

// umask has no meaning on Windows: what the daemon creates inherits the
// home's access control list.
func umask(int) int { return 0 }

// startDetached starts cmd with no console and in a process group of its
// own, so that closing the caller's console or pressing Ctrl-C there does
// not stop it. It asks to leave the caller's job too, so that a terminal
// or harness that kills its job on exit does not take the daemon with it;
// a job that does not allow that refuses, and cmd starts inside it.
// A Cmd cannot be started twice, so the second attempt is a copy and is
// what is returned.
func startDetached(cmd *exec.Cmd) (*exec.Cmd, error) {
	flags := uint32(detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags | createBreakawayFromJob, HideWindow: true}
	if err := cmd.Start(); err == nil {
		return cmd, nil
	}
	retry := exec.Command(cmd.Path, cmd.Args[1:]...)
	retry.Env, retry.Dir = cmd.Env, cmd.Dir
	retry.Stdin, retry.Stdout, retry.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr
	retry.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	return retry, retry.Start()
}

// listenLocal listens on the local API socket (AF_UNIX, Windows 10 1803
// and later). The socket file inherits the home's access control list.
func listenLocal(sock string) (net.Listener, error) {
	if len(sock) >= unixPathMax {
		return nil, fmt.Errorf("the socket path %s is longer than Windows allows (%d bytes); set A2P2_HOME to a shorter directory", sock, unixPathMax-1)
	}
	return net.Listen("unix", sock)
}
