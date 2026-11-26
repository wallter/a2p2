package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// What install does differently on Windows. It has no build tag, so that
// its tests run on every system with Windows-style paths: Options.OS says
// which system to act for. docs/HARNESSES.md has the
// sources for each harness's locations on Windows.

func (o Options) targetOS() string {
	if o.OS != "" {
		return o.OS
	}
	return runtime.GOOS
}

// shellCommand reports whether a2p2 writes the binary's path into text a
// shell runs: a hook command (Claude Code, Codex) or the commands of Pi's
// skill. The other harnesses take the command and its arguments as a list.
func shellCommand(name string) bool { return name == "claude" || name == "codex" || name == "pi" }

// forOS adjusts o for the system install acts for, or says why it cannot.
// On Unix it changes nothing.
func forOS(name string, o *Options) error {
	if o.targetOS() != "windows" {
		return nil
	}
	if name == "agy" {
		return fmt.Errorf("a2p2 install agy is not supported on Windows: where the Antigravity CLI keeps its hooks " +
			"there and which shell runs them is not documented; see docs/HARNESSES.md to add a2p2 by hand")
	}
	if !shellCommand(name) {
		return nil
	}
	// Which shell runs a hook on Windows differs (Claude Code: Git Bash,
	// or PowerShell without it; Codex: not documented), and each quotes
	// differently. A path with forward slashes and only plain characters
	// is one word as it is in every one of them, so that is the only kind
	// written, and any other is refused rather than quoted for a guess.
	bin := windowsSlashes(o.Binary)
	if !plainWord(bin) {
		// A new folder directly under C:\ inherits a rule that lets every
		// signed-in user change what is in it, so the advice names the
		// command that removes inherited access and grants this user,
		// SYSTEM and Administrators. It keeps grants made on the folder
		// itself, so the advice says to look at the list afterwards.
		return fmt.Errorf("on Windows a2p2 is named by its path in a command a shell runs, so the path may hold only "+
			"letters, digits and / . _ - : (no spaces); %s does not. Copy a2p2.exe to a directory with such a path "+
			"that only you can change, and run that copy's install. A folder you create directly under C:\\ can be "+
			"changed by every user of this computer until you run, in cmd.exe: icacls DIR /inheritance:r /grant:r "+
			"\"%%USERNAME%%:(OI)(CI)F\" \"SYSTEM:(OI)(CI)F\" \"Administrators:(OI)(CI)F\" (in PowerShell write "+
			"$env:USERNAME for %%USERNAME%%). Then icacls DIR should list no other user; remove any it does", o.Binary)
	}
	o.Binary = bin
	return nil
}

// plainWord reports whether s is one word that cmd, PowerShell and bash
// all take as it is: ASCII letters and digits and / . _ - :, not starting
// with -. PowerShell reads , and @ and cmd reads % specially, so those are
// left out although Unix shells take them.
func plainWord(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-:", r)) {
			return false
		}
	}
	return true
}

// windowsSlashes writes a Windows path with forward slashes, which
// Windows accepts and shells do not treat as escapes.
func windowsSlashes(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// hermesDir is HERMES_HOME, or its default: ~/.hermes, and on Windows
// %LOCALAPPDATA%\hermes, where Hermes's Windows installer puts it.
func hermesDir(o Options) (string, error) {
	if o.targetOS() != "windows" {
		return envOr(o, "HERMES_HOME", ".hermes")
	}
	if o.HomeDir == "" {
		if v := os.Getenv("HERMES_HOME"); v != "" {
			return v, nil
		}
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "hermes"), nil
		}
	}
	return envOr(o, "HERMES_HOME", "AppData", "Local", "hermes")
}
