package daemon

import (
	"fmt"
	"strings"
)

// This file reads a Windows security descriptor in its text form (SDDL).
// It has no build tag so that its tests run everywhere; only
// sys_windows.go calls it.

// Trustees that may hold access to a private home besides the user: the
// operating system (SYSTEM) and the administrators, who can take any file
// on the machine anyway (as root can on Unix), and the placeholders that
// stand for the owner.
var trustedSIDs = map[string]bool{
	"SY": true, "S-1-5-18": true, // LocalSystem
	"BA": true, "S-1-5-32-544": true, // BUILTIN\Administrators
	"CO": true, "S-1-3-0": true, // CREATOR OWNER (inherit-only, becomes the creator)
	"OW": true, "S-1-3-4": true, // OWNER RIGHTS
}

// homeSDDL is the security descriptor a2p2 creates its home with: owned by
// user, a protected DACL (nothing inherited from the parent) that gives
// user full control of the directory and of everything created inside.
func homeSDDL(user string) string {
	return "O:" + user + "D:P(A;OICI;FA;;;" + user + ")"
}

// checkSDDL refuses a security descriptor whose owner is not one of owners,
// or whose DACL allows anyone other than user and the trusted trustees.
// Deny entries take nothing away from this check and are ignored.
func checkSDDL(sddl, user string, owners []string, home string) error {
	owner, dacl, hasDACL, err := splitSDDL(sddl)
	if err == nil && !hasDACL {
		err = fmt.Errorf("no DACL in %q", sddl)
	}
	if err != nil {
		return fmt.Errorf("%s: cannot read its access control list: %w", home, err)
	}
	ok := false
	for _, o := range owners {
		if o != "" && strings.EqualFold(owner, o) {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("%s is owned by another user (%s)", home, owner)
	}
	fix := fmt.Sprintf("remove it and let a2p2 create it, or run: icacls \"%s\" /inheritance:r /grant:r \"%%USERNAME%%:(OI)(CI)F\" and remove the other entries", home)
	if strings.HasPrefix(dacl, "NO_ACCESS_CONTROL") {
		return fmt.Errorf("%s has no access control list, so every user can open it; %s", home, fix)
	}
	for _, ace := range aces(dacl) {
		f := strings.Split(ace, ";")
		if len(f) < 6 {
			return fmt.Errorf("%s: unreadable access control entry %q", home, ace)
		}
		switch strings.ToUpper(f[0]) {
		case "D", "OD", "XD": // deny
			continue
		}
		who := f[5]
		if strings.EqualFold(who, user) || trustedSIDs[strings.ToUpper(who)] {
			continue
		}
		return fmt.Errorf("%s is accessible to other users (%s is allowed in); %s", home, who, fix)
	}
	return nil
}

// splitSDDL returns the owner and the DACL part (after "D:") of sddl, and
// whether it has a DACL part.
func splitSDDL(sddl string) (owner, dacl string, hasDACL bool, err error) {
	parts := map[byte]string{}
	i := 0
	for i < len(sddl) {
		if i+1 >= len(sddl) || sddl[i+1] != ':' || !strings.ContainsRune("OGDS", rune(sddl[i])) {
			return "", "", false, fmt.Errorf("unexpected %q", sddl[i:])
		}
		tag := sddl[i]
		j := i + 2
		depth := 0
		for j < len(sddl) {
			c := sddl[j]
			if c == '(' {
				depth++
			} else if c == ')' {
				depth--
			} else if depth == 0 && j+1 < len(sddl) && sddl[j+1] == ':' && strings.ContainsRune("OGDS", rune(c)) {
				break
			}
			j++
		}
		parts[tag] = sddl[i+2 : j]
		i = j
	}
	dacl, hasDACL = parts['D']
	return parts['O'], dacl, hasDACL, nil
}

// aces returns the entries of a DACL's text, without their outer
// parentheses; the flags before the first entry are skipped. A conditional
// entry has parentheses of its own inside.
func aces(dacl string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(dacl); i++ {
		switch dacl[i] {
		case '(':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				out = append(out, dacl[start:i])
			}
		}
	}
	if depth > 0 {
		out = append(out, dacl[start:])
	}
	return out
}
