package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetConfigPathXDG(t *testing.T) {
	tmp := t.TempDir()
	// Isolate from the real home dotfile and any inherited config env.
	t.Setenv("HOME", tmp)
	t.Setenv("SENTRY_MCP_CONFIG", "")
	os.Unsetenv("SENTRY_MCP_CONFIG")

	// Explicit XDG_CONFIG_HOME.
	xdg := filepath.Join(tmp, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	cfgDir := filepath.Join(xdg, "sentry-mcp")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(want, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := getConfigPath(); got != want {
		t.Errorf("XDG_CONFIG_HOME path = %q, want %q", got, want)
	}

	// Default location ~/.config/sentry-mcp/config.json when XDG unset.
	os.Unsetenv("XDG_CONFIG_HOME")
	defDir := filepath.Join(tmp, ".config", "sentry-mcp")
	if err := os.MkdirAll(defDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defWant := filepath.Join(defDir, "config.json")
	if err := os.WriteFile(defWant, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := getConfigPath(); got != defWant {
		t.Errorf("default XDG path = %q, want %q", got, defWant)
	}

	// Legacy ~/.sentry-mcp.json takes precedence over XDG.
	legacy := filepath.Join(tmp, ".sentry-mcp.json")
	if err := os.WriteFile(legacy, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := getConfigPath(); got != legacy {
		t.Errorf("legacy home dotfile should win = %q, want %q", got, legacy)
	}
}

// A GUI-launched client (Claude Desktop) spawns the server without a shell, so
// "~/..." in --config / SENTRY_MCP_CONFIG reaches us unexpanded.
func TestGetConfigPathTilde(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	want := filepath.Join(tmp, ".sentry-mcp.json")

	if got := expandHome("~"); got != tmp {
		t.Errorf("expandHome(~) = %q, want %q", got, tmp)
	}
	if got := expandHome("/abs/path"); got != "/abs/path" {
		t.Errorf("expandHome should leave absolute paths alone, got %q", got)
	}

	t.Setenv("SENTRY_MCP_CONFIG", "~/.sentry-mcp.json")
	if got := getConfigPath(); got != want {
		t.Errorf("SENTRY_MCP_CONFIG tilde = %q, want %q", got, want)
	}

	os.Unsetenv("SENTRY_MCP_CONFIG")
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"sentry-mcp", "--config", "~/.sentry-mcp.json"}
	if got := getConfigPath(); got != want {
		t.Errorf("--config tilde = %q, want %q", got, want)
	}
	os.Args = []string{"sentry-mcp", "--config=~/.sentry-mcp.json"}
	if got := getConfigPath(); got != want {
		t.Errorf("--config= tilde = %q, want %q", got, want)
	}
}
