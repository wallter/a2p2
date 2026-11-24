package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The harnesses whose configuration file a2p2 edits itself: OpenCode,
// Hermes Agent and Pi (config.go).
var fileNames = Names[len(hookNames):]

func adapterPath(t *testing.T, name string, o Options) string {
	t.Helper()
	p, err := adapters[name].path(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	hermesSeed = `# my settings
model:
  default: qwen3.6:64k   # local
  provider: custom
mcp_servers:
    github:
        command: npx
        args: ["-y", "server-github"]
    # a comment inside the block
display:
  skin: dark
`
	opencodeJSONC = `{
  // the user's own file, which OpenCode created
  "$schema": "https://opencode.ai/config.json",
  "mcp": {"other": {"type": "local", "command": ["x"],},},
}
`
)

// seedFile fills a home with the user's own configuration for a harness,
// next to (or in) the file a2p2 edits.
func seedFile(t *testing.T, name string, o Options) {
	t.Helper()
	switch name {
	case "opencode":
		writeFile(t, filepath.Join(filepath.Dir(adapterPath(t, name, o)), "opencode.jsonc"), opencodeJSONC)
		writeFile(t, adapterPath(t, name, o), `{"theme":"dark","n":1.50,"mcp":{"other":{"type":"remote","url":"https://x"}}}`)
	case "hermes":
		writeFile(t, adapterPath(t, name, o), hermesSeed)
	case "pi":
		writeFile(t, filepath.Join(filepath.Dir(filepath.Dir(adapterPath(t, name, o))), "mine", "SKILL.md"), "---\nname: mine\n---\n")
	}
}

// I2 for the three file adapters: after two installs and an uninstall,
// every file and directory of the home is as it was, whether the home was
// empty or held the user's own configuration. For OpenCode the harness
// itself adds "$schema" to a config file in between, as 1.18.30 does.
func TestFileAdapterUninstallLeavesTheWholeHomeAsItWasBeforeInstall(t *testing.T) {
	for _, name := range fileNames {
		for _, seeded := range []bool{false, true} {
			o, r := setup(t)
			if seeded {
				seedFile(t, name, o)
			}
			before := tree(t, o.HomeDir)
			for range 2 {
				if _, err := Install(name, o); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			if reflect.DeepEqual(tree(t, o.HomeDir), before) {
				t.Fatalf("%s: install changed nothing", name)
			}
			if name == "opencode" && !seeded {
				p := adapterPath(t, name, o)
				b, _ := os.ReadFile(p)
				writeFile(t, p, strings.Replace(string(b), "{", `{"$schema":"https://opencode.ai/config.json",`, 1))
			}
			rep, err := Uninstall(name, o)
			if err != nil {
				t.Fatal(err)
			}
			after := tree(t, o.HomeDir)
			for k, v := range before {
				if after[k] != v {
					t.Errorf("%s seeded=%v: %s is %q after uninstall, was %q\n%s", name, seeded, k, after[k], v, rep)
				}
			}
			for k, v := range after {
				if _, ok := before[k]; !ok {
					t.Errorf("%s seeded=%v: uninstall left %s: %q\n%s", name, seeded, k, v, rep)
				}
			}
			if len(r.calls) != 0 {
				t.Errorf("%s: ran a harness CLI: %q", name, r.calls)
			}
		}
	}
}

// OpenCode: install sets exactly mcp.a2p2 in opencode.json and keeps every
// other key; the user's opencode.jsonc is never written; a file that is
// not plain JSON is refused untouched; a stale entry is replaced.
func TestOpenCodeInstallSetsOnlyItsOwnServer(t *testing.T) {
	o, _ := setup(t)
	seedFile(t, "opencode", o)
	p := adapterPath(t, "opencode", o)
	if _, err := Install("opencode", o); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"theme": "dark", "n": 1.5, "mcp": map[string]any{
		"other": map[string]any{"type": "remote", "url": "https://x"},
		"a2p2":  map[string]any{"type": "local", "command": []any{"/opt/a2p2", "mcp", "--harness", "opencode"}, "enabled": true},
	}}
	if got := readJSON(t, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("opencode.json = %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "opencode.jsonc")); string(b) != opencodeJSONC {
		t.Fatalf("opencode.jsonc changed: %s", b)
	}
	o.Binary = "/new/a2p2"
	Install("opencode", o)
	if b, _ := os.ReadFile(p); strings.Count(string(b), "a2p2") != 2 || !strings.Contains(string(b), "/new/a2p2") {
		t.Fatalf("stale entry not replaced once: %s", b)
	}

	o2, _ := setup(t)
	p2 := adapterPath(t, "opencode", o2)
	writeFile(t, p2, "{ // mine\n}")
	if _, err := Install("opencode", o2); err == nil || !strings.Contains(err.Error(), "not a plain JSON") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(p2); string(b) != "{ // mine\n}" {
		t.Fatalf("modified: %s", b)
	}
	writeFile(t, p2, `{"mcp":[]}`)
	if _, err := Install("opencode", o2); err == nil {
		t.Fatal("mcp that is not an object accepted")
	}
}

// Hermes: the a2p2 entry goes under the existing block-style mcp_servers
// with that block's indentation, a stale one is replaced, everything else
// keeps its bytes, and layouts a2p2 cannot edit as text are refused.
func TestHermesInstallEditsOnlyItsEntryInConfigYAML(t *testing.T) {
	o, _ := setup(t)
	p := adapterPath(t, "hermes", o)
	writeFile(t, p, hermesSeed)
	if _, err := Install("hermes", o); err != nil {
		t.Fatal(err)
	}
	entry := "    a2p2:\n        command: \"/opt/a2p2\"\n        args: [\"mcp\", \"--harness\", \"hermes\"]\n"
	want := strings.Replace(hermesSeed, "mcp_servers:\n", "mcp_servers:\n"+entry, 1)
	if b, _ := os.ReadFile(p); string(b) != want {
		t.Fatalf("config.yaml:\n%s", b)
	}
	if ok, cmd, err := hermesFind(o); !ok || cmd != "/opt/a2p2" || err != nil {
		t.Fatalf("find: %v %q %v", ok, cmd, err)
	}
	// Hermes rewrote the file in its own style, with an old binary.
	stale := "model:\n  default: x\nmcp_servers:\n  a2p2:\n    command: /old/a2p2\n    args:\n    - mcp\n  github:\n    command: npx\n"
	writeFile(t, p, stale)
	if _, err := Install("hermes", o); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "/old/") || strings.Count(string(b), "a2p2:") != 1 || !strings.Contains(string(b), "  github:\n    command: npx\n") {
		t.Fatalf("stale entry:\n%s", b)
	}
	if _, err := Uninstall("hermes", o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "model:\n  default: x\nmcp_servers:\n  github:\n    command: npx\n" {
		t.Fatalf("after uninstall:\n%s", b)
	}

	for body, ok := range map[string]bool{
		"model: {default: x}\n": true,
		"mcp_servers: {}\n":     true,
		"mcp_servers:\n":        true,
		"x: 1":                  true,
		"{model: x}\n":          false,
		"mcp_servers: {github: {command: npx}}\n":      false,
		"mcp_servers:\n\tgithub:\n\t\tcommand: npx\n":  false,
		"a: 1\n---\nb: 2\n":                            false,
		"mcp_servers:\n  x: 1\nmcp_servers:\n  y: 2\n": false,
		"mcp_servers:\n    x:\n      a: 1\n  y: 2\n":   false,
	} {
		o, _ := setup(t)
		p := adapterPath(t, "hermes", o)
		writeFile(t, p, body)
		_, err := Install("hermes", o)
		if (err == nil) != ok {
			t.Errorf("%q: err = %v", body, err)
			continue
		}
		if !ok {
			if b, _ := os.ReadFile(p); string(b) != body {
				t.Errorf("%q refused but modified: %s", body, b)
			}
			continue
		}
		if found, cmd, err := hermesFind(o); !found || cmd != "/opt/a2p2" || err != nil {
			b, _ := os.ReadFile(p)
			t.Errorf("%q: entry not found after install: %v %v\n%s", body, found, err, b)
		}
		if _, err := Uninstall("hermes", o); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("%q: not restored: %q", body, b)
		}
	}
}

// Pi: the skill names the binary and the CLI commands, a SKILL.md a2p2 did
// not write is refused, and nothing else is written.
func TestPiInstallWritesOnlyItsSkill(t *testing.T) {
	o, _ := setup(t)
	o.Binary = "/opt/my tools/a2p2"
	if _, err := Install("pi", o); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(adapterPath(t, "pi", o))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"name: a2p2\n", "Binary: /opt/my tools/a2p2\n", `'/opt/my tools/a2p2' inbox --as "$ME" --json`, "ack --as", "--reply-to ID", piMarker} {
		if !strings.Contains(string(b), s) {
			t.Errorf("skill lacks %q:\n%s", s, b)
		}
	}
	if ok, cmd, _ := piFind(o); !ok || cmd != o.Binary {
		t.Fatalf("find: %v %q", ok, cmd)
	}
	o2, _ := setup(t)
	p := adapterPath(t, "pi", o2)
	writeFile(t, p, "my own a2p2 skill")
	if _, err := Install("pi", o2); err == nil {
		t.Fatal("foreign skill overwritten")
	}
	if rep, err := Uninstall("pi", o2); err != nil || !strings.Contains(rep, "no skill found") {
		t.Fatalf("%q %v", rep, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "my own a2p2 skill" {
		t.Fatalf("foreign skill changed: %s", b)
	}
}

func TestFileAdapterDryRunAndMissingCLI(t *testing.T) {
	for _, name := range fileNames {
		o, _ := setup(t)
		o.DryRun = true
		before := tree(t, o.HomeDir)
		rep, err := Install(name, o)
		if err != nil || !strings.HasPrefix(rep, "would ") {
			t.Errorf("%s: %q %v", name, rep, err)
		}
		if !reflect.DeepEqual(tree(t, o.HomeDir), before) {
			t.Errorf("%s: dry run wrote", name)
		}
		o.DryRun = false
		Install(name, o)
		o.DryRun = true
		mid := tree(t, o.HomeDir)
		if rep, err := Uninstall(name, o); err != nil || !strings.Contains(rep, "would remove") {
			t.Errorf("%s: %q %v", name, rep, err)
		}
		if !reflect.DeepEqual(tree(t, o.HomeDir), mid) {
			t.Errorf("%s: dry-run uninstall wrote", name)
		}
		o.LookPath = func(string) (string, error) { return "", errors.New("nope") }
		if _, err := Install(name, o); err == nil || !strings.Contains(err.Error(), "not on PATH") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Doctor reports each of the three as not connected, connected, or
// pointing at another binary; for OpenCode it also reads an entry the
// user put in opencode.jsonc, comments and trailing commas included.
func TestDoctorChecksTheFileAdapters(t *testing.T) {
	o, _ := setup(t)
	o.LookPath = func(f string) (string, error) {
		if _, ok := adapters[f]; !ok && f != "opencode" && f != "hermes" && f != "pi" {
			return "", errors.New("no")
		}
		return f, nil
	}
	checks := func() map[string]Check {
		m := map[string]Check{}
		for _, c := range Doctor(o) {
			m[c.Name] = c
		}
		return m
	}
	cs := checks()
	if len(cs) != 3 {
		t.Fatalf("checks: %+v", cs)
	}
	for _, c := range cs {
		if !c.OK || !strings.Contains(c.Info, "not connected") {
			t.Errorf("before: %+v", c)
		}
	}
	for _, name := range fileNames {
		if _, err := Install(name, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range checks() {
		if !c.OK || !strings.Contains(c.Info, "in place") {
			t.Errorf("installed: %+v", c)
		}
	}
	o.Binary = "/other/a2p2"
	for _, c := range checks() {
		if c.OK || !strings.Contains(c.Info, "different") || c.Fix == "" {
			t.Errorf("other binary: %+v", c)
		}
	}
	o2, _ := setup(t)
	o2.LookPath = o.LookPath
	jsonc := filepath.Join(filepath.Dir(adapterPath(t, "opencode", o2)), "opencode.jsonc")
	writeFile(t, jsonc, "{\n  // mine\n  \"mcp\": {\"a2p2\": {\"command\": [\"/opt/a2p2\", \"mcp\",], /* x */},},\n  \"s\": \"// not a comment\",\n}\n")
	if ok, cmd, err := opencodeFind(o2); !ok || cmd != "/opt/a2p2" || err != nil {
		t.Fatalf("jsonc: %v %q %v", ok, cmd, err)
	}
	var v map[string]any
	if err := json.Unmarshal(stripJSONC([]byte(`{"a":"x//y","b":[1,2,],}`)), &v); err != nil || v["a"] != "x//y" {
		t.Fatalf("strip: %v %v", v, err)
	}
}
