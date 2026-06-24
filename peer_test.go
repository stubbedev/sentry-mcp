package main

import (
	"encoding/json"
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

func TestIdKey(t *testing.T) {
	cases := map[string]string{
		`"srv-1"`: "srv-1", // string id
		`42`:      "42",    // numeric id
	}
	for raw, want := range cases {
		if got := idKey(json.RawMessage(raw)); got != want {
			t.Errorf("idKey(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestPeerListRootsHeaderWins(t *testing.T) {
	p := &peer{reg: newPendingRegistry()}
	p.markRootsCapable()
	p.setHeaderRoots([]mcpRoot{{URI: "/srv/header"}})

	// send is nil and headerRoots is set, so listRoots must not attempt a call.
	roots := p.listRoots()
	if len(roots) != 1 || roots[0].URI != "/srv/header" {
		t.Fatalf("header roots not returned: %+v", roots)
	}

	// list_changed must not clear header-pinned roots.
	p.invalidateRoots()
	if got := p.listRoots(); len(got) != 1 {
		t.Errorf("header roots cleared by invalidate: %+v", got)
	}
}
