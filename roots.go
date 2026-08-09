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
// X-Repo-Root leads: it is the name the rest of this fleet reads and the one
// the Claude Code entries send, and headers are the only workspace signal that
// survives MCP 2026-07-28 (see resolveRoots).
var rootHeaders = []string{"X-Repo-Root", "X-Mcp-Roots", "X-Mcp-Root", "Mcp-Roots", "Mcp-Root"}

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
	// Only ask when the client both advertised roots and negotiated a protocol
	// version where a server may still ask (see rootsRemovedFrom).
	if !rootsUsable(req.Session) {
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

// rootsRemovedFrom is the first protocol revision that forbids server-initiated
// JSON-RPC requests (SEP-2322 / SEP-2575): from there on roots/list is not
// something a server can ask for. Clients on that revision pin the workspace
// with one of the rootHeaders instead. ISO dates compare correctly as strings.
const rootsRemovedFrom = "2026-07-28"

// rootsUsable reports whether this session may still be asked for its roots:
// the client advertised the capability, on a protocol version that still allows
// the question.
func rootsUsable(ss *mcp.ServerSession) bool {
	if ss == nil {
		return false
	}
	return rootsAllowed(ss.InitializeParams())
}

// rootsAllowed is rootsUsable's decision, split out so it can be tested without
// a live session.
func rootsAllowed(ip *mcp.InitializeParams) bool {
	if ip == nil || ip.Capabilities == nil || ip.ProtocolVersion >= rootsRemovedFrom {
		return false
	}
	return ip.Capabilities.RootsV2 != nil
}
