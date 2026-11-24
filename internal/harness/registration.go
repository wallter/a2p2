package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Where each harness's own CLI keeps the MCP registration (user scope), as
// observed with Claude Code 2.1.331, codex-cli 0.171.0 and agy 1.5.16 in a
// throwaway home. Doctor only reads them; registering stays the CLIs' job.
var mcpFiles = map[string][]string{
	"claude": {".claude.json"},
	"codex":  {".codex", "config.toml"},
	"agy":    {".gemini", "config", "mcp_config.json"},
}

// codexTable is the TOML table codex mcp add writes for the server.
const codexTable = "[mcp_servers.a2p2]"

// codexApproval is the one line Install adds to that table: Codex refuses
// MCP tool calls without approval (headless, and by default since 0.169
// for unannotated tools). It names this server only, and the trailing
// comment marks it as a2p2's, so Uninstall removes exactly it.
const codexApproval = `default_tools_approval_mode = "approve" # added by a2p2 install`

func codexConfig(o Options) (string, error) {
	home, err := o.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, mcpFiles["codex"]...)...), nil
}

// grantCodexApproval adds codexApproval directly under a2p2's table. It
// reports false when the table is not there (codex wrote it differently),
// so the caller can say so instead of guessing.
func grantCodexApproval(o Options) (bool, error) {
	path, err := codexConfig(o)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != codexTable {
			continue
		}
		for _, x := range lines[i+1:] {
			if strings.HasPrefix(strings.TrimSpace(x), "[") {
				break
			}
			if strings.TrimSpace(x) == codexApproval {
				return true, nil
			}
		}
		out := append(append(append([]string{}, lines[:i+1]...), codexApproval), lines[i+1:]...)
		return true, writeKeepingMode(path, []byte(strings.Join(out, "\n")))
	}
	return false, nil
}

// revokeCodexApproval removes every line Install added, wherever it is,
// and nothing else. It reports whether it removed one.
func revokeCodexApproval(o Options) (bool, error) {
	path, err := codexConfig(o)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var keep []string
	lines := strings.Split(string(b), "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) != codexApproval {
			keep = append(keep, l)
		}
	}
	if len(keep) == len(lines) {
		return false, nil
	}
	return true, writeKeepingMode(path, []byte(strings.Join(keep, "\n")))
}

// writeKeepingMode replaces path atomically, keeping its permission bits.
// A symlink is followed (see resolve); a dangling one is refused.
func writeKeepingMode(path string, b []byte) error {
	path, err := resolve(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".a2p2-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	// Windows has no permission bits to keep (File.Chmod fails there); the
	// new file takes its directory's access control list.
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(mode); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// registered reports whether the harness's own config registers the a2p2
// MCP server, and the command it runs ("" when unknown).
func registered(name string, o Options) (found bool, command string, err error) {
	home, err := o.home()
	if err != nil {
		return false, "", err
	}
	path := filepath.Join(append([]string{home}, mcpFiles[name]...)...)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if name == "codex" {
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if strings.TrimSpace(l) != codexTable {
				continue
			}
			for _, x := range lines[i+1:] {
				x = strings.TrimSpace(x)
				if strings.HasPrefix(x, "[") {
					break
				}
				if k, v, ok := strings.Cut(x, "="); ok && strings.TrimSpace(k) == "command" {
					var s string
					if json.Unmarshal([]byte(strings.TrimSpace(v)), &s) == nil {
						command = s
					}
				}
			}
			return true, command, nil
		}
		return false, "", nil
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return false, "", err
	}
	s, ok := doc.MCPServers["a2p2"]
	return ok, s.Command, nil
}
