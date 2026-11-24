package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// An install record is kept in a2p2's own state directory
// ($A2P2_HOME/installs/<harness>.json, or Options.StateDir), never in the
// harness's files. It says that a2p2 installed into that harness and what
// entry named a2p2, if any, the first install replaced, so that uninstall
// removes only what a2p2 added and puts that entry back. Without a record
// uninstall removes nothing.
type record struct {
	Harness string `json:"harness"`
	File    string `json:"file"`            // where the replaced entry lived
	Had     bool   `json:"had"`             // an entry named a2p2 was there before the first install
	Prior   string `json:"prior,omitempty"` // that entry: JSON for JSON files, the lines of its table or block otherwise
}

func (o Options) stateDir() (string, error) {
	if o.StateDir != "" {
		return o.StateDir, nil
	}
	if h := os.Getenv("A2P2_HOME"); h != "" {
		return filepath.Join(h, "installs"), nil
	}
	home, err := o.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".a2p2", "installs"), nil
}

func recordPath(name string, o Options) (string, error) {
	d, err := o.stateDir()
	return filepath.Join(d, name+".json"), err
}

// loadRecord returns the harness's install record, or nil when there is none.
func loadRecord(name string, o Options) (*record, error) {
	p, err := recordPath(name, o)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s is not a valid install record (%v); move it aside to continue", p, err)
	}
	return &r, nil
}

func saveRecord(r *record, o Options) error {
	p, err := recordPath(r.Harness, o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	return writeKeepingMode(p, append(b, '\n'))
}

func dropRecord(name string, o Options) error {
	p, err := recordPath(name, o)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(p)) // only if empty
	return nil
}

func noRecord(name string, o Options) string {
	p, _ := recordPath(name, o)
	return "no install record at " + p + ": a2p2 did not install into " + name +
		" from this state directory, so it removed nothing. Remove an entry named a2p2 yourself if it is a2p2's"
}

// --- the entry named a2p2 in the files the harness CLIs own ---

// cliEntry reads the a2p2 entry the harness CLI keeps (mcpFiles): the JSON
// value of mcpServers.a2p2, or for Codex the text of the [mcp_servers.a2p2]
// table and its sub-tables, without the line a2p2 adds.
func cliEntry(name string, o Options) (path string, had bool, prior string, err error) {
	home, err := o.home()
	if err != nil {
		return "", false, "", err
	}
	path = filepath.Join(append([]string{home}, mcpFiles[name]...)...)
	b, ok, err := readMaybe(path)
	if err != nil || !ok {
		return path, false, "", err
	}
	if name == "codex" {
		_, table := codexTables(string(b))
		return path, table != "", table, nil
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return path, false, "", fmt.Errorf("%s is not a JSON object; a2p2 will not install over it", path)
	}
	var servers map[string]json.RawMessage
	_ = json.Unmarshal(doc["mcpServers"], &servers)
	if e, ok := servers["a2p2"]; ok {
		return path, true, string(e), nil
	}
	return path, false, "", nil
}

func isCodexA2P2Header(l string) bool {
	t := strings.TrimSpace(l)
	return t == codexTable || strings.HasPrefix(t, "[mcp_servers.a2p2.")
}

// codexTables splits config.toml into the text without a2p2's tables and
// those tables (without the approval line a2p2 adds).
func codexTables(s string) (rest, table string) {
	var keep, ours []string
	in := false
	for _, l := range strings.SplitAfter(s, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "[") {
			in = isCodexA2P2Header(l)
		}
		switch {
		case in && strings.TrimSpace(l) == codexApproval:
		case in:
			ours = append(ours, l)
		default:
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, ""), strings.Join(ours, "")
}

// putBackCLIEntry writes the recorded entry back after the harness CLI
// removed a2p2's own one.
func putBackCLIEntry(name string, r *record) error {
	b, ok, err := readMaybe(r.File)
	if err != nil {
		return err
	}
	if name == "codex" {
		s := string(b)
		rest, _ := codexTables(s) // drop whatever the CLI left of a2p2's table
		if rest = strings.TrimRight(rest, "\n"); rest != "" {
			rest += "\n\n"
		}
		return writeKeepingMode(r.File, []byte(rest+r.Prior))
	}
	doc := map[string]json.RawMessage{}
	if ok && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s is no longer a JSON object; put this back as mcpServers.a2p2 yourself: %s", r.File, r.Prior)
		}
	}
	servers := map[string]json.RawMessage{}
	_ = json.Unmarshal(doc["mcpServers"], &servers)
	servers["a2p2"] = json.RawMessage(r.Prior)
	doc["mcpServers"], _ = json.Marshal(servers)
	if !ok {
		if err := os.MkdirAll(filepath.Dir(r.File), 0o755); err != nil {
			return err
		}
	}
	return writeKeepingMode(r.File, encodeJSON(doc))
}

// --- symlinks ---

// A configuration path that is a symlink is followed: a2p2 writes the file
// it points to and leaves the link in place, for every harness. A dangling
// symlink is refused before anything is written, since writing would
// replace the link with a file.
func resolve(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s is a symlink to a missing file; a2p2 will not replace the link. Fix or remove it, then run again", path)
	}
	return r, nil
}
