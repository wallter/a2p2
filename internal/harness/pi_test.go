package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// piCommands returns the skill's command lines that run the binary, each
// with the heredoc body that follows it, if any.
func piCommands(skill, bin string) []string {
	end := piEnd([]byte(skill))
	var out []string
	lines := strings.Split(skill, "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if !strings.HasPrefix(l, quote(bin)+" ") {
			continue
		}
		cmd := l
		if strings.Contains(l, "<<'") {
			for i++; i < len(lines); i++ {
				cmd += "\n" + lines[i]
				if lines[i] == end {
					break
				}
			}
		}
		out = append(out, cmd)
	}
	return out
}

// F16: message text reaches a2p2 send only through a heredoc with a quoted
// end word, never inside double quotes, and the label is written out in
// every command instead of a variable that the next command may not have.
func TestPiSkillSendsTextThroughQuotedHeredoc(t *testing.T) {
	skill := PiSkill("/opt/a2p2", piTextEnd+"_0123abcd")
	if strings.Contains(skill, `"text"`) || strings.Contains(skill, "$ME") {
		t.Fatalf("the skill still puts text in double quotes or uses $ME:\n%s", skill)
	}
	cmds := piCommands(skill, "/opt/a2p2")
	var sends int
	for _, c := range cmds {
		first, _, _ := strings.Cut(c, "\n")
		if strings.Contains(first, " status ") {
			continue
		}
		if !strings.Contains(first, `--as "pi-$(basename "$PWD")"`) {
			t.Errorf("a command without the label written out: %q", first)
		}
		if strings.Contains(first, " send ") {
			sends++
			if !strings.HasSuffix(first, " - <<'"+piTextEnd+"_0123abcd'") {
				t.Errorf("a send that does not read a quoted heredoc: %q", first)
			}
		}
	}
	if sends == 0 {
		t.Fatalf("no send command in the skill:\n%s", skill)
	}
}

// F16, run in bash: hostile text in the heredoc reaches a2p2 as it is and
// runs nothing, and each command, run in a fresh shell, gets its label.
func TestPiSkillCommandsRunNothingFromMessageTextInBash(t *testing.T) {
	sh, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs bash")
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(dir, "proj")
	os.Mkdir(proj, 0o755)
	bin := filepath.Join(dir, "fake a2p2")
	args, in := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+quote(args)+"\ncat > "+quote(in)+"\n"), 0o755)
	// The text also holds the end word without the digits, and an end word
	// of the same form, each followed by a command: neither ends the
	// heredoc of an installed skill, whose digits the text cannot know.
	hostile := "quote me: $(touch PWNED) `touch PWNED2` ${HOME} \"x\" 'y'\nsecond line \\\n" +
		piTextEnd + "\ntouch PWNED3\n" + piTextEnd + "_00000000\ntouch PWNED4\n"
	o, _ := setup(t)
	o.Binary = bin
	oldRoots, oldStop := tempRoots, stopAt
	tempRoots, stopAt = func() []string { return nil }, filepath.Dir(bin)
	defer func() { tempRoots, stopAt = oldRoots, oldStop }()
	if _, err := Install("pi", o); err != nil {
		t.Fatal(err)
	}
	p, _ := piPath(o)
	skill, _ := os.ReadFile(p)
	end := piEnd(skill)
	if end == "" || end == piTextEnd+"_00000000" {
		t.Fatalf("installed skill has end word %q", end)
	}
	var sent bool
	for _, c := range piCommands(string(skill), bin) {
		first, _, _ := strings.Cut(c, "\n")
		if strings.Contains(first, "<<'") {
			c = first + "\n" + hostile + end
		}
		os.Remove(args)
		cmd := exec.Command(sh, "-c", c)
		cmd.Dir = proj
		cmd.Stdin = strings.NewReader("")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%q: %v %s", c, err, out)
		}
		a, _ := os.ReadFile(args)
		if !strings.Contains(first, " status ") && !strings.Contains(string(a), "--as\npi-proj\n") {
			t.Errorf("%q gave arguments %q", first, a)
		}
		if strings.Contains(first, " send ") {
			sent = true
			if b, _ := os.ReadFile(in); string(b) != hostile {
				t.Errorf("text reached a2p2 as %q, want %q", b, hostile)
			}
		}
	}
	for _, f := range []string{"PWNED", "PWNED2", "PWNED3", "PWNED4"} {
		if _, err := os.Stat(filepath.Join(proj, f)); err == nil {
			t.Fatalf("bash ran a command from the message text (%s)", f)
		}
	}
	if !sent {
		t.Fatal("no send was run")
	}
}

// F16: the end word is random per install and kept on a reinstall, and
// doctor fails a skill an older a2p2 wrote, which put text in double
// quotes, until install writes the current one.
func TestPiSkillEndWordIsPerInstallAndDoctorFailsAnOlderSkill(t *testing.T) {
	o, _ := setup(t)
	o.Run = fakeCLI(o.HomeDir)
	if _, err := Install("pi", o); err != nil {
		t.Fatal(err)
	}
	p, _ := piPath(o)
	first, _ := os.ReadFile(p)
	if _, err := Install("pi", o); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(p); string(again) != string(first) {
		t.Fatal("a reinstall changed the end word")
	}
	o2, _ := setup(t)
	Install("pi", o2)
	p2, _ := piPath(o2)
	if other, _ := os.ReadFile(p2); piEnd(other) == piEnd(first) {
		t.Fatalf("two installs chose the same end word %q", piEnd(first))
	}
	if c := adapters["pi"].doctor("pi", o); !c.OK {
		t.Fatalf("doctor on the current skill: %+v", c)
	}
	old := "---\nname: a2p2\n---\n" + piMarker + "\n\nBinary: " + o.Binary + "\n\n```sh\nME=\"pi-$(basename \"$PWD\")\"\n" +
		o.Binary + " send --as \"$ME\" --reply-to ID \"text\"\n```\n"
	writeFile(t, p, old)
	if c := adapters["pi"].doctor("pi", o); c.OK || !strings.Contains(c.Info, "older a2p2") || c.Fix != "a2p2 install pi" {
		t.Fatalf("doctor on an older skill: %+v", c)
	}
	if _, err := Install("pi", o); err != nil {
		t.Fatal(err)
	}
	if c := adapters["pi"].doctor("pi", o); !c.OK {
		t.Fatalf("doctor after reinstall: %+v", c)
	}
}
