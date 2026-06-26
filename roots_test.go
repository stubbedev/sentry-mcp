package main

import (
	"net/http"
	"testing"
)

func TestMcpRootPath(t *testing.T) {
	cases := map[string]string{
		"file:///srv/myrepo": "/srv/myrepo",
		"/srv/plain":         "/srv/plain",
		"file://":            "file://", // no path component, returned as-is
	}
	for uri, want := range cases {
		if got := (mcpRoot{URI: uri}).path(); got != want {
			t.Errorf("path(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestParseRootHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Mcp-Root", "file:///srv/a")
	h.Add("X-Mcp-Roots", "/srv/b, /srv/c")

	roots := parseRootHeaders(h)
	if len(roots) != 3 {
		t.Fatalf("got %d roots, want 3: %+v", len(roots), roots)
	}
	// Order follows the rootHeaders precedence list (X-Mcp-Roots before X-Mcp-Root).
	want := []string{"/srv/b", "/srv/c", "file:///srv/a"}
	for i, w := range want {
		if roots[i].URI != w {
			t.Errorf("root[%d] = %q, want %q", i, roots[i].URI, w)
		}
	}

	if got := parseRootHeaders(http.Header{}); got != nil {
		t.Errorf("empty headers should yield nil, got %+v", got)
	}
}
