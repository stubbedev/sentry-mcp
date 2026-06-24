package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// SentryConfig holds the resolved Sentry connection settings.
type SentryConfig struct {
	URL   string
	Token string
	Org   string
}

// Config is the top-level resolved configuration.
type Config struct {
	Sentry *SentryConfig
}

type configFile struct {
	Sentry struct {
		URL   string `json:"url"`
		Token string `json:"token"`
		Org   string `json:"org"`
	} `json:"sentry"`
}

func readJSONFile(path string) *configFile {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cf configFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil
	}
	return &cf
}

// getConfigPath resolves the config file location in priority order:
// --config <path> CLI arg → SENTRY_MCP_CONFIG → ~/.sentry-mcp.json → ./.sentry-mcp.json
func getConfigPath() string {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			p, _ := filepath.Abs(args[i+1])
			return p
		}
		if strings.HasPrefix(a, "--config=") {
			p, _ := filepath.Abs(strings.TrimPrefix(a, "--config="))
			return p
		}
	}
	if env := os.Getenv("SENTRY_MCP_CONFIG"); env != "" {
		p, _ := filepath.Abs(env)
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		homeConfig := filepath.Join(home, ".sentry-mcp.json")
		if fileExists(homeConfig) {
			return homeConfig
		}
	}
	// XDG location: $XDG_CONFIG_HOME/sentry-mcp/config.json (default ~/.config).
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		if home, err := os.UserHomeDir(); err == nil {
			xdg = filepath.Join(home, ".config")
		}
	}
	if xdg != "" {
		xdgConfig := filepath.Join(xdg, "sentry-mcp", "config.json")
		if fileExists(xdgConfig) {
			return xdgConfig
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		cwdConfig := filepath.Join(cwd, ".sentry-mcp.json")
		if fileExists(cwdConfig) {
			return cwdConfig
		}
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// loadDotEnv loads KEY=VALUE pairs from a .env file in cwd into the process
// environment, without overwriting variables that are already set. Mirrors the
// behaviour of dotenv used by the previous TypeScript implementation.
func loadDotEnv() {
	raw, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// loadConfig resolves configuration from the config file then environment
// variables, validating that all three Sentry fields are present and the URL
// parses. Returns a Config with a nil Sentry when configuration is incomplete.
func loadConfig() Config {
	loadDotEnv()

	var file *configFile
	if path := getConfigPath(); path != "" {
		file = readJSONFile(path)
	}

	pick := func(fileVal, env string) string {
		if fileVal != "" {
			return fileVal
		}
		return os.Getenv(env)
	}

	var sURL, token, org string
	if file != nil {
		sURL = pick(file.Sentry.URL, "SENTRY_URL")
		token = pick(file.Sentry.Token, "SENTRY_AUTH_TOKEN")
		org = pick(file.Sentry.Org, "SENTRY_ORG_SLUG")
	} else {
		sURL = os.Getenv("SENTRY_URL")
		token = os.Getenv("SENTRY_AUTH_TOKEN")
		org = os.Getenv("SENTRY_ORG_SLUG")
	}

	cfg := Config{}
	if sURL != "" && token != "" && org != "" {
		if _, err := url.ParseRequestURI(sURL); err != nil {
			logf("Invalid SENTRY_URL: %s", sURL)
			return cfg
		}
		cfg.Sentry = &SentryConfig{
			URL:   strings.TrimRight(sURL, "/"),
			Token: token,
			Org:   org,
		}
	} else if sURL != "" || token != "" || org != "" {
		var missing []string
		if sURL == "" {
			missing = append(missing, "sentry.url (or SENTRY_URL)")
		}
		if token == "" {
			missing = append(missing, "sentry.token (or SENTRY_AUTH_TOKEN)")
		}
		if org == "" {
			missing = append(missing, "sentry.org (or SENTRY_ORG_SLUG)")
		}
		logf("Sentry disabled: missing %s", strings.Join(missing, ", "))
	}

	return cfg
}
