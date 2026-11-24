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
	if err := checkSDDL(homeSDDL(me), me, []string{me}, `C:\h`); err != nil {
		t.Fatal(err)
	}
	// What a user profile directory usually inherits: SYSTEM,
	// Administrators and the user, plus a conditional entry for the user.
	inherited := "O:" + me + "D:AI(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;" + me + ")" +
		"(XA;OICIID;FA;;;" + me + ";(@User.x == 1))(D;OICI;FA;;;WD)(A;OICIIO;FA;;;CO)"
	if err := checkSDDL(inherited, me, []string{me}, `C:\h`); err != nil {
		t.Fatal(err)
	}
	// An elevated administrator's default owner may be Administrators.
	if err := checkSDDL("O:BA"+"D:P(A;OICI;FA;;;"+me+")", me, []string{me, "BA"}, `C:\h`); err != nil {
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
		err := checkSDDL(sddl, me, []string{me}, `C:\h`)
		if err == nil || !strings.Contains(err.Error(), "accessible to other users") && !strings.Contains(err.Error(), "no access control list") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWindowsHomeOwnedByAnotherUserIsRefused(t *testing.T) {
	err := checkSDDL("O:"+other+"D:P(A;OICI;FA;;;"+me+")", me, []string{me, ""}, `C:\h`)
	if err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "O:" + me, "garbage", "X:1"} {
		if err := checkSDDL(bad, me, []string{me}, `C:\h`); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
