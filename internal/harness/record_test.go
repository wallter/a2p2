package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// userEntry puts the person's own server named a2p2 where each harness
// keeps it, and returns a check that it is there again, unchanged.
func userEntry(t *testing.T, name string, o Options) (check func() bool) {
	t.Helper()
	switch name {
	case "claude", "agy":
		p := filepath.Join(append([]string{o.HomeDir}, mcpFiles[name]...)...)
		writeFile(t, p, `{"theme":"dark","mcpServers":{"a2p2":{"command":"/usr/local/bin/mine","args":["--x"],"env":{"K":"v"}},"other":{"command":"o"}}}`)
		return func() bool {
			var d struct {
				MCPServers map[string]any `json:"mcpServers"`
			}
			b, _ := os.ReadFile(p)
			json.Unmarshal(b, &d)
			return reflect.DeepEqual(d.MCPServers["a2p2"], map[string]any{"command": "/usr/local/bin/mine", "args": []any{"--x"}, "env": map[string]any{"K": "v"}}) &&
				d.MCPServers["other"] != nil
		}
	case "codex":
		p := filepath.Join(append([]string{o.HomeDir}, mcpFiles[name]...)...)
		table := "[mcp_servers.a2p2]\ncommand = \"/usr/local/bin/mine\"\n\n[mcp_servers.a2p2.env]\nK = \"v\"\n"
		writeFile(t, p, "model = \"o3\"\n\n"+table)
		return func() bool {
			b, _ := os.ReadFile(p)
			return strings.Contains(string(b), table) && strings.Count(string(b), "[mcp_servers.a2p2]") == 1 &&
				!strings.Contains(string(b), "/opt/a2p2") && strings.Contains(string(b), "model = \"o3\"\n")
		}
	case "opencode":
		p := adapterPath(t, name, o)
		writeFile(t, p, `{"mcp":{"a2p2":{"type":"remote","url":"https://mine.example"}}}`)
		return func() bool {
			m, _ := readJSON(t, p).(map[string]any)["mcp"].(map[string]any)
			return reflect.DeepEqual(m["a2p2"], map[string]any{"type": "remote", "url": "https://mine.example"})
		}
	case "hermes":
		p := adapterPath(t, name, o)
		writeFile(t, p, "mcp_servers:\n  a2p2:\n    command: /usr/local/bin/mine # mine\n")
		return func() bool {
			ok, cmd, _ := hermesFind(o)
			b, _ := os.ReadFile(p)
			return ok && cmd == "/usr/local/bin/mine" && strings.Contains(string(b), "/usr/local/bin/mine # mine")
		}
	}
	return nil
}

// I2, blocker D: install over the person's own entry named a2p2 records
// it in a2p2's state; uninstall puts it back, keeps the person's later
// unrelated edits, and removes the record. The backup of a file that has
// changed since is kept. For all five harnesses that register a server
// (Pi's skill is never installed over a file a2p2 did not write).
func TestUninstallPutsBackTheEntryNamedA2P2ThatInstallReplaced(t *testing.T) {
	for _, name := range []string{"claude", "codex", "agy", "opencode", "hermes"} {
		for _, edited := range []bool{false, true} {
			o, _ := setup(t)
			o.Run = fakeCLI(o.HomeDir)
			back := userEntry(t, name, o)
			rep, err := Install(name, o)
			if err != nil || !strings.Contains(rep, "uninstall puts it back") {
				t.Fatalf("%s: %v %s", name, err, rep)
			}
			if back() {
				t.Fatalf("%s: install did not replace the entry", name)
			}
			Install(name, o) // a second install keeps the first record
			if edited {      // the person adds a server of their own afterwards
				switch name {
				case "claude", "agy":
					p := filepath.Join(append([]string{o.HomeDir}, mcpFiles[name]...)...)
					d := readJSON(t, p).(map[string]any)
					d["later"] = true
					b, _ := json.Marshal(d)
					writeFile(t, p, string(b))
				case "codex":
					p := filepath.Join(append([]string{o.HomeDir}, mcpFiles[name]...)...)
					b, _ := os.ReadFile(p)
					writeFile(t, p, "later = 1\n"+string(b))
				case "opencode":
					p := adapterPath(t, name, o)
					d := readJSON(t, p).(map[string]any)
					d["mcp"].(map[string]any)["later"] = map[string]any{"type": "remote", "url": "https://later"}
					b, _ := json.Marshal(d)
					writeFile(t, p, string(b))
				case "hermes":
					p := adapterPath(t, name, o)
					b, _ := os.ReadFile(p)
					writeFile(t, p, string(b)+"  later:\n    command: later\n")
				}
			}
			rep, err = Uninstall(name, o)
			if err != nil {
				t.Fatal(err)
			}
			if !back() {
				p := filepath.Join(append([]string{o.HomeDir}, mcpFiles[name]...)...)
				if adapters[name].path != nil {
					p = adapterPath(t, name, o)
				}
				b, _ := os.ReadFile(p)
				t.Fatalf("%s edited=%v: the person's entry is not back:\n%s\n%s", name, edited, b, rep)
			}
			if r, _ := loadRecord(name, o); r != nil {
				t.Errorf("%s: record left", name)
			}
			if edited {
				p := filepath.Join(append([]string{o.HomeDir}, mcpFiles[name]...)...)
				if adapters[name].path != nil {
					p = adapterPath(t, name, o)
				}
				if b, _ := os.ReadFile(p); !strings.Contains(string(b), "later") {
					t.Errorf("%s: the later edit is gone:\n%s", name, b)
				}
			}
			if edited && adapters[name].path != nil {
				p := adapterPath(t, name, o)
				if _, err := os.Stat(p + bakSuffix); err != nil || !strings.Contains(rep, "kept "+p+bakSuffix) {
					t.Errorf("%s: the backup of a changed file was not kept: %v\n%s", name, err, rep)
				}
			}
		}
	}
}

// Blocker D: uninstall without an install record removes nothing, runs
// no harness CLI, and says so, for all six harnesses.
func TestUninstallWithoutARecordRemovesNothing(t *testing.T) {
	for _, name := range Names {
		o, r := setup(t)
		if name != "pi" {
			userEntry(t, name, o)
		} else {
			writeFile(t, adapterPath(t, name, o), PiSkill("/opt/a2p2")) // an older a2p2's skill
		}
		if _, ok := adapters[name]; !ok {
			writeFile(t, hookPath(name, o), `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/opt/a2p2 hook --harness `+name+` --event UserPromptSubmit"}]}]}}`)
		}
		before := tree(t, o.HomeDir)
		rep, err := Uninstall(name, o)
		if err != nil || !strings.Contains(rep, "removed nothing") {
			t.Errorf("%s: %v %q", name, err, rep)
		}
		if !reflect.DeepEqual(tree(t, o.HomeDir), before) || len(r.calls) != 0 {
			t.Errorf("%s: uninstall without a record changed the home or ran %q", name, r.calls)
		}
	}
}

// Blocker D (Hermes): every layout either gets a correct edit, which
// parses back with a2p2's entry and every original line, and uninstall
// returns the exact bytes; or is refused with nothing written.
func TestHermesEditNeverTurnsAValidFileIntoAnInvalidOne(t *testing.T) {
	for _, c := range []struct {
		body string
		ok   bool
	}{
		{"mcp_servers:", true}, // no final newline
		{"model: x", true},
		{"mcp_servers: # note\n", true},
		{"mcp_servers: {} # empty\n", true},
		{"mcp_servers:\n", true},
		{"model:\r\n  default: x\r\nmcp_servers:\r\n  other:\r\n    command: o\r\n", true},
		{"model:\r\n  default: x\r\n", true},
		{"# only a comment", true},
		{"---\nmodel: x\n", true},
		{"mcp_servers:\n  other:\n    command: o\ndisplay:\n  skin: dark", true},
		{"", true},
		{"model:\r\n  a: 1\nb: 2\r\n", false}, // mixed line endings
		{"mcp_servers: {github: {command: npx}}\n", false},
		{"mcp_servers:\n\tgithub: x\n", false},
		{"- a\n- b\n", false},
		{"just a scalar\n", false},
		{"  indented: 1\n", false},
		{"a: 1\n---\nb: 2\n", false},
		{"mcp_servers: {}\n  x: 1\n", false},
		{"mcp_servers:\n  - a\n", false},
	} {
		o, _ := setup(t)
		p := adapterPath(t, "hermes", o)
		writeFile(t, p, c.body)
		_, err := Install("hermes", o)
		if (err == nil) != c.ok {
			t.Errorf("%q: err = %v", c.body, err)
			continue
		}
		b, _ := os.ReadFile(p)
		if !c.ok {
			if string(b) != c.body {
				t.Errorf("%q refused but changed: %q", c.body, b)
			}
			continue
		}
		y, err := parseHermes(b)
		if err != nil || y.entry < 0 {
			t.Errorf("%q: result does not parse back: %v\n%q", c.body, err, b)
			continue
		}
		if ok, cmd, _ := hermesFind(o); !ok || cmd != "/opt/a2p2" {
			t.Errorf("%q: entry %v %q in %q", c.body, ok, cmd, b)
		}
		if strings.Contains(c.body, "\r\n") && strings.Count(string(b), "\n") != strings.Count(string(b), "\r\n") {
			t.Errorf("%q: line endings not kept: %q", c.body, b)
		}
		orig, _ := parseHermes([]byte(c.body))
		if err := checkHermes(b, y.lines[y.entry:y.eod], orig.outside()); err != nil {
			t.Errorf("%q: %v\n%q", c.body, err, b)
		}
		if _, err := Uninstall("hermes", o); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(p); string(b) != c.body || (err != nil && c.body != "") {
			t.Errorf("%q: after uninstall %q %v", c.body, b, err)
		}
	}
}

// Blocker D (OpenCode, Pi): a file a2p2 cannot edit exactly is refused and
// left as it was: opencode.json with comments or trailing commas (JSONC),
// a non-object, an "mcp" that is not an object; a SKILL.md a2p2 did not
// write. opencode.jsonc is never edited.
func TestOpenCodeAndPiRefuseWhatTheyCannotEditExactly(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"opencode", "{\n  // mine\n  \"theme\": \"x\",\n}\n"},
		{"opencode", "[1, 2]\n"},
		{"opencode", "{\"mcp\": [1]}\n"},
		{"opencode", "{\"a\": 1} {\"b\": 2}\n"},
		{"pi", "---\nname: a2p2\n---\nmine\n"},
	} {
		o, r := setup(t)
		p := adapterPath(t, c.name, o)
		writeFile(t, p, c.body)
		before := tree(t, o.HomeDir)
		if _, err := Install(c.name, o); err == nil {
			t.Errorf("%s %q: accepted", c.name, c.body)
		}
		if !reflect.DeepEqual(tree(t, o.HomeDir), before) || len(r.calls) != 0 {
			t.Errorf("%s %q: changed the home", c.name, c.body)
		}
		if rec, _ := loadRecord(c.name, o); rec != nil {
			t.Errorf("%s %q: record saved for a refused install", c.name, c.body)
		}
	}
	o, _ := setup(t)
	jsonc, _ := opencodeFile(o, "opencode.jsonc")
	writeFile(t, jsonc, "{\n  // mine\n  \"mcp\": {\"a2p2\": {\"type\": \"remote\", \"url\": \"u\"},},\n}\n")
	if _, err := Install("opencode", o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(jsonc); !strings.Contains(string(b), "// mine") {
		t.Errorf("opencode.jsonc edited: %s", b)
	}
}

// Should-fix D: a dangling symlink at any config path a2p2 would write is
// refused before anything is written or run; a symlink to an existing
// file is followed, and the link stays. Same rule for all six.
func TestConfigSymlinksAreFollowedAndDanglingOnesRefused(t *testing.T) {
	for _, name := range Names {
		o, r := setup(t)
		p := hookPath(name, o)
		if _, ok := adapters[name]; ok {
			p = adapterPath(t, name, o)
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), p); err != nil {
			t.Fatal(err)
		}
		before := tree(t, o.HomeDir)
		if _, err := Install(name, o); err == nil || !strings.Contains(err.Error(), "symlink to a missing file") {
			t.Errorf("%s: dangling link: err = %v", name, err)
		}
		if !reflect.DeepEqual(tree(t, o.HomeDir), before) || len(r.calls) != 0 {
			t.Errorf("%s: dangling link: wrote or ran %q", name, r.calls)
		}

		o2, _ := setup(t)
		o2.Run = fakeCLI(o2.HomeDir)
		p2 := strings.Replace(p, o.HomeDir, o2.HomeDir, 1)
		target := filepath.Join(t.TempDir(), "dotfiles", "conf")
		body := map[string]string{"hermes": "model:\n  default: x\n", "pi": PiSkill("/old/a2p2")}[name]
		if body == "" {
			body = `{"theme":"dark"}`
		}
		writeFile(t, target, body)
		os.MkdirAll(filepath.Dir(p2), 0o755)
		os.Symlink(target, p2)
		if _, err := Install(name, o2); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st, err := os.Lstat(p2); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: link replaced", name)
		}
		if b, _ := os.ReadFile(target); !strings.Contains(string(b), "/opt/a2p2") {
			t.Fatalf("%s: target not written: %s", name, b)
		}
		if _, err := Uninstall(name, o2); err != nil {
			t.Fatal(err)
		}
		if st, err := os.Lstat(p2); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: link replaced by uninstall", name)
		}
	}
	// Codex's config.toml, which a2p2 also edits.
	o, _ := setup(t)
	cfg, _ := codexConfig(o)
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.Symlink(filepath.Join(t.TempDir(), "missing"), cfg)
	if _, err := Install("codex", o); err == nil {
		t.Error("codex: dangling config.toml accepted")
	}
}
