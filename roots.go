package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpRoot is a workspace root exposed by the MCP client (the "roots" feature).
// It carries the repo/working-tree location the harness wants the server to
// operate in — the input a shell-calling tool would need.
type mcpRoot struct {
	URI  string
	Name string
}

// path returns the filesystem path for a file:// root, or the raw value when it
// is already a plain path.
func (r mcpRoot) path() string {
	if strings.HasPrefix(r.URI, "file://") {
		if u, err := url.Parse(r.URI); err == nil && u.Path != "" {
			return u.Path
		}
	}
	return r.URI
}

// rootFromString builds a root from a header value, which may be a file:// URI
// or a plain path.
func rootFromString(s string) (mcpRoot, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return mcpRoot{}, false
	}
	return mcpRoot{URI: s}, true
}

// rootHeaders are the request headers a proxy/harness may set to hand the
// server the workspace root(s) without the MCP roots round-trip. Values are
// file:// URIs or plain paths; multiple roots may be comma-separated.
var rootHeaders = []string{"X-Mcp-Roots", "X-Mcp-Root", "Mcp-Roots", "Mcp-Root"}

// parseRootHeaders collects roots from the recognized request headers.
func parseRootHeaders(h http.Header) []mcpRoot {
	var roots []mcpRoot
	for _, name := range rootHeaders {
		for _, v := range h.Values(name) {
			for _, part := range strings.Split(v, ",") {
				if r, ok := rootFromString(part); ok {
					roots = append(roots, r)
				}
			}
		}
	}
	return roots
}

// resolveRoots returns the client's workspace roots for the in-flight call.
// Header-pinned roots (set by a proxy/harness over HTTP) take precedence; else
// the roots are fetched from the client session via roots/list.
func resolveRoots(ctx context.Context, req *mcp.CallToolRequest) []mcpRoot {
	if req == nil {
		return nil
	}
	if req.Extra != nil && req.Extra.Header != nil {
		if roots := parseRootHeaders(req.Extra.Header); len(roots) > 0 {
			return roots
		}
	}
	if req.Session == nil {
		return nil
	}
	res, err := req.Session.ListRoots(ctx, &mcp.ListRootsParams{})
	if err != nil || res == nil {
		return nil
	}
	out := make([]mcpRoot, 0, len(res.Roots))
	for _, r := range res.Roots {
		out = append(out, mcpRoot{URI: r.URI, Name: r.Name})
	}
	return out
}
