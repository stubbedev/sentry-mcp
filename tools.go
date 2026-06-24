package main

import (
	_ "embed"
	"encoding/json"
)

//go:embed tools.json
var toolsJSON string

//go:embed package.json
var pkgJSON []byte

// versionFromPkg reads the version field from the embedded package.json — the
// single source of truth for the server version across all build paths.
func versionFromPkg() string {
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(pkgJSON, &p); err == nil && p.Version != "" {
		return p.Version
	}
	return "dev"
}
