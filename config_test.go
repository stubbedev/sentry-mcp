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
