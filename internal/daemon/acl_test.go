package daemon

import (
	"strings"
	"testing"
)

// The Windows home check reads the home's security descriptor as text, so
// it is tested here on every system; sys_windows_test.go checks real
// directories on Windows.

const (
	me    = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	other = "S-1-5-21-1111111111-2222222222-3333333333-1002"
)

func TestWindowsHomeCreatedByA2p2IsAcceptedAsPrivate(t *testing.T) {
	if err := checkSDDL(homeSDDL(me), me, []string{me}, `C:\h`, nil); err != nil {
		t.Fatal(err)
	}
	// What a user profile directory usually inherits: SYSTEM,
	// Administrators and the user, plus a conditional entry for the user.
	inherited := "O:" + me + "D:AI(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;" + me + ")" +
		"(XA;OICIID;FA;;;" + me + ";(@User.x == 1))(D;OICI;FA;;;WD)(A;OICIIO;FA;;;CO)"
	if err := checkSDDL(inherited, me, []string{me}, `C:\h`, nil); err != nil {
		t.Fatal(err)
	}
	// An elevated administrator's default owner may be Administrators.
	if err := checkSDDL("O:BA"+"D:P(A;OICI;FA;;;"+me+")", me, []string{me, "BA"}, `C:\h`, nil); err != nil {
		t.Fatal(err)
	}
}

// The first Windows run: the runner's account is the machine's built-in
// Administrator (RID 500), which SDDL writes as LA, as owner and as the
// trustee of its own entries. Compared as written, the home looked owned
// by another user; compared resolved it is the user's own.
func TestWindowsHomeOfTheBuiltInAdministratorWrittenAsLAIsItsOwn(t *testing.T) {
	admin := "S-1-5-21-1111111111-2222222222-3333333333-500"
	sid := func(s string) string {
		switch s {
		case "LA":
			return admin
		case "BA":
			return "S-1-5-32-544"
		case "SY":
			return "S-1-5-18"
		}
		return s
	}
	for _, sddl := range []string{
		"O:LAD:P(A;OICI;FA;;;LA)",
		"O:LAD:AI(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;LA)",
	} {
		if err := checkSDDL(sddl, admin, []string{admin, admin}, `C:\h`, sid); err != nil {
			t.Fatalf("%s: %v", sddl, err)
		}
	}
	// Resolving takes nothing away: another account written as a constant
	// (LG, the built-in Guest) is still another user.
	sid2 := func(s string) string {
		if s == "LG" {
			return "S-1-5-21-1111111111-2222222222-3333333333-501"
		}
		return sid(s)
	}
	if err := checkSDDL("O:LAD:P(A;OICI;FA;;;LA)(A;;FR;;;LG)", admin, []string{admin}, `C:\h`, sid2); err == nil ||
		!strings.Contains(err.Error(), "LG = S-1-5-21-1111111111-2222222222-3333333333-501") {
		t.Fatal(err)
	}
	if err := checkSDDL("O:LGD:P(A;OICI;FA;;;LA)", admin, []string{admin}, `C:\h`, sid2); err == nil ||
		!strings.Contains(err.Error(), "owned by another user") {
		t.Fatal(err)
	}
}

func TestWindowsHomeOthersCanOpenIsRefused(t *testing.T) {
	for name, sddl := range map[string]string{
		"everyone":            "O:" + me + "D:(A;OICI;FA;;;WD)",
		"users":               "O:" + me + "D:AI(A;OICIID;FA;;;" + me + ")(A;OICIID;0x1200a9;;;BU)",
		"authenticated users": "O:" + me + "D:(A;;FR;;;AU)(A;OICI;FA;;;" + me + ")",
		"another user":        "O:" + me + "D:P(A;OICI;FA;;;" + me + ")(A;OICI;FR;;;" + other + ")",
		"inherit only":        "O:" + me + "D:P(A;OICI;FA;;;" + me + ")(A;OICIIO;FA;;;" + other + ")",
		"conditional":         "O:" + me + "D:P(XA;;FA;;;WD;(@User.x == 1))",
		"null dacl":           "O:" + me + "D:NO_ACCESS_CONTROL",
	} {
		err := checkSDDL(sddl, me, []string{me}, `C:\h`, nil)
		if err == nil || !strings.Contains(err.Error(), "accessible to other users") && !strings.Contains(err.Error(), "no access control list") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWindowsHomeOwnedByAnotherUserIsRefused(t *testing.T) {
	err := checkSDDL("O:"+other+"D:P(A;OICI;FA;;;"+me+")", me, []string{me, ""}, `C:\h`, nil)
	if err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "O:" + me, "garbage", "X:1"} {
		if err := checkSDDL(bad, me, []string{me}, `C:\h`, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
