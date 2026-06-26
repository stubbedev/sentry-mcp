package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Default HTTP bind address and endpoint path. The default host is loopback so
// that, behind a reverse proxy, the server is not inadvertently exposed; set an
// explicit host (e.g. 0.0.0.0:8765) to listen on all interfaces.
const (
	defaultHTTPAddr = "127.0.0.1:8765"
	defaultHTTPPath = "/mcp"

	sessionTTL = 30 * time.Minute // idle sessions are reaped after this
)

// httpAddr resolves the HTTP listen address. HTTP mode is enabled when --http
// (optionally --http=addr) is passed or SENTRY_MCP_HTTP_ADDR is set. A bare
// --http or a truthy SENTRY_MCP_HTTP uses defaultHTTPAddr. Returns "" for the
// default stdio transport.
func httpAddr() string {
	args := os.Args[1:]
	for i, a := range args {
		switch {
		case a == "--http":
			if next := args[i+1:]; len(next) > 0 && !strings.HasPrefix(next[0], "-") && strings.Contains(next[0], ":") {
				return next[0]
			}
			return defaultHTTPAddr
		case strings.HasPrefix(a, "--http="):
			if v := strings.TrimPrefix(a, "--http="); v != "" {
				return v
			}
			return defaultHTTPAddr
		}
	}
	if v := os.Getenv("SENTRY_MCP_HTTP_ADDR"); v != "" {
		return v
	}
	if v := strings.ToLower(os.Getenv("SENTRY_MCP_HTTP")); v == "1" || v == "true" || v == "yes" {
		return defaultHTTPAddr
	}
	return ""
}

// httpPath resolves the endpoint path (--http-path / SENTRY_MCP_HTTP_PATH).
func httpPath() string {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--http-path" {
			if next := args[i+1:]; len(next) > 0 {
				return ensureLeadingSlash(next[0])
			}
		}
		if strings.HasPrefix(a, "--http-path=") {
			return ensureLeadingSlash(strings.TrimPrefix(a, "--http-path="))
		}
	}
	if v := os.Getenv("SENTRY_MCP_HTTP_PATH"); v != "" {
		return ensureLeadingSlash(v)
	}
	return defaultHTTPPath
}

func ensureLeadingSlash(p string) string {
	if p == "" {
		return defaultHTTPPath
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// serveHTTP runs the MCP server over the SDK's Streamable HTTP transport on a
// single endpoint, with idle-session reaping. The handler reads the
// Mcp-Session-Id header, manages the SSE stream for server→client requests
// (roots/list), and exposes the request headers to tool handlers (header-pinned
// roots). Shuts down when ctx is cancelled.
func serveHTTP(ctx context.Context, srv *mcp.Server, addr, path string) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{SessionTimeout: sessionTTL},
	)

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logf("listening on http://%s%s (MCP Streamable HTTP)", addr, path)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
