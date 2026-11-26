package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realPathOf resolves the directories of p, as the writers do for a
// symlinked temporary directory (macOS's /var).
func realPathOf(p string) string {
	if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(d, filepath.Base(p))
	}
	return p
}

// target is the file a2p2 edits for a harness, other than the CLI's own.
func target(t *testing.T, name string, o Options) string {
	if _, ok := adapters[name]; ok {
		return adapterPath(t, name, o)
	}
	return hookPath(name, o)
}

// guardSetup is a home with the harness's own file present (Pi's skill is
// a2p2's, so it starts missing) and a fake harness CLI.
func guardSetup(t *testing.T, name string) Options {
	o, _ := setup(t)
	o.Run = fakeCLI(o.HomeDir)
	if _, ok := adapters[name]; ok {
		seedFile(t, name, o)
	} else {
		seed(t, name, o)
	}
	return o
}

// onCheckOf calls f the first time a guarded write checks path.
func onCheckOf(t *testing.T, path string, f func()) (checked func() int) {
	n := 0
	beforeCheck = func(p string) {
		if realPathOf(p) != realPathOf(path) {
			return
		}
		if n++; n == 1 && f != nil {
			f()
		}
	}
	t.Cleanup(func() { beforeCheck = func(string) {} })
	return func() int { return n }
}

const theirs = "{\"theirs\": true}\n"

// J1: every file a2p2 edits for a harness, the hook files and the three
// adapters' files, is read again just before the rename. An edit another
// program makes after a2p2 read the file is kept, install refuses with a
// message that names the file, and the backup a2p2 made for that write is
// taken back. Before, only Codex's config.toml was compared.
func TestAnEditMadeJustBeforeInstallWritesIsKeptForEveryHarness(t *testing.T) {
	for _, name := range Names {
		t.Run(name, func(t *testing.T) {
			o := guardSetup(t, name)
			p := target(t, name, o)
			onCheckOf(t, p, func() { writeFile(t, p, theirs) })
			_, err := Install(name, o)
			if !errors.Is(err, errChanged) || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "left it as it is") {
				t.Fatalf("install: %v", err)
			}
			if b, _ := os.ReadFile(p); string(b) != theirs {
				t.Fatalf("the other edit was lost:\n%s", b)
			}
			for _, m := range []string{bakSuffix, newSuffix} {
				if _, err := os.Stat(p + m); err == nil {
					t.Errorf("%s left behind after a refused write", m)
				}
			}
		})
	}
}

// J1: the same for uninstall, which writes or removes the file.
func TestAnEditMadeJustBeforeUninstallWritesIsKeptForEveryHarness(t *testing.T) {
	for _, name := range Names {
		t.Run(name, func(t *testing.T) {
			o := guardSetup(t, name)
			if _, err := Install(name, o); err != nil {
				t.Fatal(err)
			}
			p := target(t, name, o)
			onCheckOf(t, p, func() { writeFile(t, p, theirs) })
			if _, err := Uninstall(name, o); !errors.Is(err, errChanged) {
				t.Fatalf("uninstall: %v", err)
			}
			if b, _ := os.ReadFile(p); string(b) != theirs {
				t.Fatalf("the other edit was lost:\n%s", b)
			}
		})
	}
}

// J1, the honest case: with nothing else editing the files, every install
// and uninstall writes, and each write to the harness's file was compared
// just before its rename.
func TestEveryConfigWriteIsComparedJustBeforeItsRename(t *testing.T) {
	for _, name := range Names {
		t.Run(name, func(t *testing.T) {
			o := guardSetup(t, name)
			p := target(t, name, o)
			checked := onCheckOf(t, p, nil)
			if _, err := Install(name, o); err != nil {
				t.Fatal(err)
			}
			if checked() == 0 {
				t.Fatal("install wrote without comparing")
			}
			b, _ := os.ReadFile(p)
			if !strings.Contains(string(b), "a2p2") {
				t.Fatalf("install did not write:\n%s", b)
			}
			n := checked()
			if _, err := Uninstall(name, o); err != nil {
				t.Fatal(err)
			}
			if checked() == n {
				t.Fatal("uninstall wrote without comparing")
			}
		})
	}
}

// J1: putting back the person's own entry named a2p2 in a file the
// harness CLI owns is guarded too.
func TestPuttingBackTheEntryKeepsAnEditMadeJustBefore(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			o, _ := setup(t)
			p := filepath.Join(append([]string{o.HomeDir}, mcpFiles[name]...)...)
			body, prior := `{"mcpServers":{}}`, `{"command":"/mine"}`
			if name == "codex" {
				body, prior = "model = \"o3\"\n", codexTable+"\ncommand = \"/mine\"\n"
			}
			writeFile(t, p, body)
			r := &record{Harness: name, File: p, Had: true, Prior: prior}
			onCheckOf(t, p, func() { writeFile(t, p, theirs) })
			if err := putBackCLIEntry(name, r); !errors.Is(err, errChanged) {
				t.Fatalf("put back: %v", err)
			}
			if b, _ := os.ReadFile(p); string(b) != theirs {
				t.Fatalf("the other edit was lost:\n%s", b)
			}
			beforeCheck = func(string) {}
			writeFile(t, p, body)
			if err := putBackCLIEntry(name, r); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(p); !strings.Contains(string(b), "/mine") {
				t.Fatalf("not put back:\n%s", b)
			}
		})
	}
}

// codexWith is a config.toml with a2p2's table under header and the lines.
func codexWith(header string, lines ...string) string {
	return "model = \"o3\"\n\n" + header + "\ncommand = \"/opt/a2p2\"\n" + strings.Join(lines, "\n") + "\n"
}

// J1: --approve-tools sees the person's own default_tools_approval_mode in
// every spelling TOML allows, quoted, escaped or spaced, under a header in
// any spelling, and adds no second key, which would make Codex refuse the
// whole file. Doctor reads the same key. Before, an escaped key was missed
// and a duplicate written.
func TestCodexApproveToolsSeesTheKeyInEveryTOMLSpelling(t *testing.T) {
	for _, c := range []struct{ header, key string }{
		{codexTable, `"default_tools_approval_mode" = "prompt"`},
		{codexTable, `"default_tools_approval\u005fmode" = "prompt"`},
		{codexTable, `"\U00000064efault_tools_approval_mode"="prompt"`},
		{codexTable, `'default_tools_approval_mode'   =   'prompt'  # mine`},
		{`[mcp_servers."\u0061\u0032p2"]`, `default_tools_approval_mode = "prompt"`},
		{`[ "mcp_servers" . 'a2p2' ] # mine`, `"default_tools_approval_mode" = "pro\u006dpt"`},
	} {
		t.Run(c.key, func(t *testing.T) {
			o, _ := setup(t)
			o.ApproveTools = true
			path, _ := codexConfig(o)
			mine := codexWith(c.header, c.key)
			writeFile(t, path, mine)
			rep, err := setCodexApproval(o)
			if err != nil || !strings.Contains(strings.Join(rep, "\n"), "a2p2 added nothing") {
				t.Fatalf("%v %q", err, rep)
			}
			if b, _ := os.ReadFile(path); string(b) != mine {
				t.Fatalf("config.toml changed:\n%s", b)
			}
			if ch := codexApprovalCheck(o); !ch.OK || !strings.Contains(ch.Info, `default_tools_approval_mode = "prompt"`) {
				t.Fatalf("doctor: %+v", ch)
			}
		})
	}
}

// J1, the honest case: a header and keys in quoted or escaped spellings
// that a2p2 reads in full get the approval line once, under that header,
// and uninstall's revoke removes it again.
func TestCodexApproveToolsWritesUnderAHeaderInAnySpelling(t *testing.T) {
	o, _ := setup(t)
	o.ApproveTools = true
	path, _ := codexConfig(o)
	header := `[mcp_servers."\u00612p2"]`
	mine := "model = \"o3\"\n" + header + "\n\"command\" = \"/opt/a2p2\"\n'args' = [\"mcp\"]\n" +
		"[mcp_servers.a2p2.env]\nK = \"v\"\n"
	writeFile(t, path, mine)
	if _, err := setCodexApproval(o); err != nil {
		t.Fatal(err)
	}
	b := string(must(os.ReadFile(path)))
	if strings.Count(b, codexApproval) != 1 || !strings.Contains(b, header+"\n"+codexApproval+"\n") {
		t.Fatalf("approval not written once under the header:\n%s", b)
	}
	if c := codexApprovalCheck(o); !c.OK || !strings.Contains(c.Info, "you chose") {
		t.Fatalf("doctor: %+v", c)
	}
	if removed, err := revokeCodexApproval(o); err != nil || !removed {
		t.Fatalf("%v %v", removed, err)
	}
	if b := string(must(os.ReadFile(path))); b != mine {
		t.Fatalf("revoke left:\n%s", b)
	}
}

// J1: when a line that could set something in a2p2's server is one a2p2
// does not read in full (an inline table, one over several lines, or a
// key it cannot decode), --approve-tools writes nothing, also on a dry
// run, and tells the person the line to add by hand. Doctor says it
// cannot tell. Before, the line was added next to what it could not see.
func TestCodexApproveToolsRefusesAConfigItCannotFullyRead(t *testing.T) {
	for _, c := range []struct {
		name, body string
		line       int
	}{
		{"inline table in a2p2's table", codexWith(codexTable, `tools = { send = { approval_mode = "approve" } }`), 5},
		{"inline table over several lines", codexWith(codexTable, "tools = {", `  send = { approval_mode = "approve" },`, "}"), 5},
		{"a2p2's server as an inline table", "[mcp_servers]\na2p2 = { command = \"/opt/a2p2\", default_tools_approval_mode = \"prompt\" }\n", 2},
		{"a key a2p2 cannot decode", codexWith(codexTable, `"default\qmode" = "prompt"`), 5},
		{"a header a2p2 cannot read", "[mcp_servers.\"\\q\"]\nx = 1\n" + codexWith(codexTable), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, dry := range []bool{true, false} {
				o, _ := setup(t)
				o.ApproveTools, o.DryRun = true, dry
				path, _ := codexConfig(o)
				writeFile(t, path, c.body)
				rep, err := setCodexApproval(o)
				r := strings.Join(rep, "\n")
				if err != nil || !strings.Contains(r, fmt.Sprintf("cannot read line %d of %s", c.line, path)) ||
					!strings.Contains(r, "nothing was approved") || !strings.Contains(r, `default_tools_approval_mode = "approve"`) {
					t.Fatalf("dry=%v: %v %s", dry, err, r)
				}
				if b, _ := os.ReadFile(path); string(b) != c.body {
					t.Fatalf("dry=%v: config.toml changed:\n%s", dry, b)
				}
				if ch := codexApprovalCheck(o); ch.OK || !strings.Contains(ch.Info, "cannot tell") {
					t.Fatalf("doctor: %+v", ch)
				}
			}
		})
	}
}

// J2: doctor finds approvals the person wrote in any spelling TOML allows:
// an escaped header, a spaced dotted key, an escaped sub-table and value.
func TestDoctorReadsCodexApprovalsInEveryTOMLSpelling(t *testing.T) {
	o, _ := setup(t)
	path, _ := codexConfig(o)
	writeFile(t, path, "[mcp_servers.\"\\u00612p2\"]\ncommand = \"/opt/a2p2\"\n\"default_tools_approval_mode\" = \"appro\\u0076e\"\n"+
		"tools . \"send\" . approval_mode = 'approve'\n[mcp_servers.a2p2.tools.\"in\\u0062ox\"]\napproval_mode = \"approve\"\n")
	c := codexApprovalCheck(o)
	if !c.OK || !strings.Contains(c.Info, "not written by a2p2") || !strings.Contains(c.Info, "one by one in "+path+": send, inbox") {
		t.Fatalf("%+v", c)
	}
	// Multi-line strings on one line are read as what they hold. Before,
	// doctor said a2p2 approved no tools.
	writeFile(t, path, codexWith(codexTable, `default_tools_approval_mode = """approve"""`,
		`tools.send.approval_mode = '''approve'''`))
	if c := codexApprovalCheck(o); !c.OK || !strings.Contains(c.Info, "not written by a2p2") ||
		!strings.Contains(c.Info, "one by one in "+path+": send") {
		t.Fatalf("one-line multi-line strings: %+v", c)
	}
	// One that goes on past its line, or holds a quote or a backslash, is
	// not decoded: doctor says it cannot tell, not that nothing is approved.
	for _, l := range [][]string{
		{`default_tools_approval_mode = """`, `approve"""`},
		{`default_tools_approval_mode = '''`, `approve'''`},
		{`default_tools_approval_mode = """appro\u0076e"""`},
		{`default_tools_approval_mode = '''approve''''`},
		{`tools.send.approval_mode = """approve"""""`},
	} {
		writeFile(t, path, codexWith(codexTable, l...))
		if c := codexApprovalCheck(o); c.OK || !strings.Contains(c.Info, "cannot tell") || !strings.Contains(c.Info, "line 5 of") {
			t.Fatalf("%q: %+v", l, c)
		}
	}
}

// J1: a2p2's env table, written inline as Codex's own docs show it, or a
// key a2p2 cannot decode in a table that cannot hold an approval, does not
// stop --approve-tools, and doctor still reads the config. Before, every
// sub-table of a2p2's server counted, and both refused.
func TestCodexApproveToolsReadsPastTablesThatCannotApprove(t *testing.T) {
	for _, body := range []string{
		"[mcp_servers.a2p2]\ncommand = \"/opt/a2p2\"\nargs = [\"mcp\"]\nenv = { \"A2P2_HOME\" = \"/tmp/x\" }\n",
		"[mcp_servers.a2p2]\ncommand = \"/opt/a2p2\"\n[mcp_servers.a2p2.env]\n\"\\q\" = 1\n",
		"[mcp_servers.a2p2]\ncommand = \"/opt/a2p2\"\n[mcp_servers.a2p2.tools.send.x]\n\"\\q\" = 1\n",
	} {
		t.Run(body, func(t *testing.T) {
			o, _ := setup(t)
			o.ApproveTools = true
			path, _ := codexConfig(o)
			writeFile(t, path, body)
			if c := codexApprovalCheck(o); !c.OK || !strings.Contains(c.Info, "approved no tools") {
				t.Fatalf("doctor before: %+v", c)
			}
			rep, err := setCodexApproval(o)
			if err != nil || !strings.Contains(strings.Join(rep, "\n"), "tool approval: let every a2p2 tool") {
				t.Fatalf("%v %q", err, rep)
			}
			b := string(must(os.ReadFile(path)))
			if strings.Count(b, codexApproval) != 1 || !strings.HasPrefix(b, codexTable+"\n"+codexApproval+"\n") {
				t.Fatalf("approval not written once under the table:\n%s", b)
			}
			if c := codexApprovalCheck(o); !c.OK || !strings.Contains(c.Info, "you chose") {
				t.Fatalf("doctor after: %+v", c)
			}
		})
	}
	// The tables that can hold an approval still refuse a key a2p2 cannot
	// decode.
	for _, header := range []string{"[mcp_servers.a2p2.tools]", "[mcp_servers.a2p2.tools.send]"} {
		o, _ := setup(t)
		o.ApproveTools = true
		path, _ := codexConfig(o)
		body := codexWith(codexTable) + header + "\n\"\\q\" = 1\n"
		writeFile(t, path, body)
		if rep, _ := setCodexApproval(o); !strings.Contains(strings.Join(rep, "\n"), "nothing was approved") {
			t.Fatalf("%s: %q", header, rep)
		}
		if b := string(must(os.ReadFile(path))); b != body {
			t.Fatalf("%s: config.toml changed:\n%s", header, b)
		}
	}
}

// J2: for every harness, an install without --approve-tools, and its dry
// run, writes nothing that approves a tool, and the reports say so; for
// Codex doctor says a2p2 approved none.
func TestNoHarnessIsApprovedAnyToolByDefault(t *testing.T) {
	for _, name := range Names {
		t.Run(name, func(t *testing.T) {
			for _, dry := range []bool{true, false} {
				o, _ := setup(t)
				o.Run, o.DryRun = fakeCLI(o.HomeDir), dry
				rep, err := Install(name, o)
				if err != nil {
					t.Fatal(err)
				}
				if name == "codex" && !strings.Contains(rep, "a2p2 approves none") {
					t.Fatalf("report: %s", rep)
				}
				if name == "opencode" && !strings.Contains(rep, "a2p2 approves none") {
					t.Fatalf("report: %s", rep)
				}
				for p, v := range tree(t, o.HomeDir) {
					if strings.HasSuffix(p, newSuffix) {
						continue // lists the directories install made, named after this test
					}
					for _, w := range []string{"approv", "allow", "permission", "trust"} {
						if strings.Contains(strings.ToLower(v), w) {
							t.Errorf("dry=%v: %s holds %q:\n%s", dry, p, w, v)
						}
					}
				}
				if name == "codex" {
					if c := codexApprovalCheck(o); !c.OK || (!dry && !strings.Contains(c.Info, "approved no tools")) {
						t.Fatalf("doctor: %+v", c)
					}
				}
			}
		})
	}
}
