package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Install for Windows, on any system: Options.OS is "windows" and the
// binary has a Windows path.

// skipWithoutSymlinks skips a test on a Windows account that may not make
// symbolic links (that needs Developer Mode or an administrator).
func skipWithoutSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	d := t.TempDir()
	if err := os.Symlink(filepath.Join(d, "x"), filepath.Join(d, "l")); err != nil {
		t.Skipf("this Windows account cannot make symbolic links: %v", err)
	}
}

func windowsOpts(t *testing.T, bin string) (Options, *rec) {
	o, r := setup(t)
	o.OS, o.Binary = "windows", bin
	return o, r
}

func TestWindowsHookCommandIsOnePlainWordForEveryShell(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		o, r := windowsOpts(t, `C:\Users\me\go\bin\a2p2.exe`)
		if _, err := Install(name, o); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(hookPath(name, o))
		if err != nil {
			t.Fatal(err)
		}
		want := "C:/Users/me/go/bin/a2p2.exe hook --harness " + name + " --event UserPromptSubmit"
		if !strings.Contains(string(b), `"`+want+`"`) {
			t.Fatalf("%s hook file:\n%s", name, b)
		}
		if !isOurs(name, want) {
			t.Fatalf("%s: own command not recognised", name)
		}
		if !strings.Contains(strings.Join(r.calls, "\n"), "-- C:/Users/me/go/bin/a2p2.exe mcp --harness "+name) {
			t.Fatalf("%s mcp add: %v", name, r.calls)
		}
		o.OS = "windows"
		for _, c := range Doctor(o) {
			if strings.HasPrefix(c.Name, name+" hook") && !c.OK {
				t.Fatalf("doctor after install: %+v", c)
			}
		}
		if _, err := Uninstall(name, o); err != nil {
			t.Fatal(err)
		}
		if count(t, hookPath(name, o), name) != 0 {
			t.Fatalf("%s: hook left after uninstall", name)
		}
	}
}

func TestWindowsInstallRefusesABinaryPathEachShellWouldQuoteDifferently(t *testing.T) {
	for _, bin := range []string{`C:\Program Files\a2p2\a2p2.exe`, `C:\Users\Jo Smith\bin\a2p2.exe`,
		`C:\a,b\a2p2.exe`, `C:\%x%\a2p2.exe`, `C:\it's\a2p2.exe`, ""} {
		for _, name := range []string{"claude", "codex", "pi"} {
			o, r := windowsOpts(t, bin)
			if _, err := Install(name, o); err == nil || !strings.Contains(err.Error(), "no spaces") {
				t.Fatalf("%s %q: %v", name, bin, err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("%s %q: ran %v", name, bin, r.calls)
			}
			if _, err := os.Stat(hookPath("claude", o)); !os.IsNotExist(err) {
				t.Fatalf("%s %q: wrote a hook file", name, bin)
			}
		}
	}
}

func TestWindowsInstallSaysAntigravityIsNotEstablishedThere(t *testing.T) {
	o, r := windowsOpts(t, `C:\a2p2\a2p2.exe`)
	if _, err := Install("agy", o); err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("ran %v", r.calls)
	}
}

func TestWindowsListHarnessesKeepTheBinaryPathAsItIs(t *testing.T) {
	bin := `C:\Program Files\a2p2\a2p2.exe` // a list of arguments: no shell, spaces are fine
	o, _ := windowsOpts(t, bin)
	if _, err := Install("opencode", o); err != nil {
		t.Fatal(err)
	}
	p, _ := opencodeFile(o, "opencode.json")
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `"C:\\Program Files\\a2p2\\a2p2.exe"`) {
		t.Fatalf("opencode config:\n%s", b)
	}
	if _, err := Install("hermes", o); err != nil {
		t.Fatal(err)
	}
	// Hermes's Windows installer keeps its home in %LOCALAPPDATA%\hermes.
	hp := filepath.Join(o.HomeDir, "AppData", "Local", "hermes", "config.yaml")
	if b, err := os.ReadFile(hp); err != nil || !strings.Contains(string(b), `C:\\Program Files`) {
		t.Fatalf("hermes config %s: %v\n%s", hp, err, b)
	}
}

func TestWindowsPiSkillNamesTheBinaryWithForwardSlashes(t *testing.T) {
	o, _ := windowsOpts(t, `C:\a2p2\a2p2.exe`)
	if _, err := Install("pi", o); err != nil {
		t.Fatal(err)
	}
	p, _ := piPath(o)
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "\nC:/a2p2/a2p2.exe inbox --as") || strings.Contains(string(b), `\a2p2`) {
		t.Fatalf("skill: %v\n%s", err, b)
	}
}

func TestUnixInstallIsUnchangedByTheWindowsRules(t *testing.T) {
	o, _ := setup(t)
	o.OS, o.Binary = "linux", "/opt/my dir/a2p2"
	if err := forOS("claude", &o); err != nil || o.Binary != "/opt/my dir/a2p2" {
		t.Fatalf("%v %q", err, o.Binary)
	}
	if err := forOS("agy", &o); err != nil {
		t.Fatal(err)
	}
}
