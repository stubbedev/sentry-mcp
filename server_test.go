package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServerToolRoundTrip wires the real server (tool registration + dispatch +
// CallToolResult conversion) to an in-memory client and exercises tools/list
// plus a tools/call against a stub Sentry API. It is the single check that the
// SDK conversion is hooked up end to end.
func TestServerToolRoundTrip(t *testing.T) {
	// Stub Sentry: /auth/ identifies the user, used by getDevContext.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/0/auth/") {
			w.Write([]byte(`{"id":"1","username":"abs","email":"abs@example.com"}`))
			return
		}
		w.Write([]byte(`[]`)) // projects, etc.
	}))
	defer stub.Close()

	sentry = NewSentryClient(stub.URL, "tok", "konform")
	defer func() { sentry = nil }()

	srv := mcp.NewServer(&mcp.Implementation{Name: "sentry-mcp", Version: "test"}, nil)
	registerTools(srv)
	if len(validators) != 9 {
		t.Fatalf("got %d resolved validators, want 9 (a schema failed to resolve)", len(validators))
	}

	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 9 {
		t.Fatalf("got %d tools, want 9", len(tools.Tools))
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "sentry_get_dev_context"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("dev context returned error: %+v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "Sentry instance") {
		t.Fatalf("unexpected dev context output: %+v", res.Content)
	}

	// Missing required arg (issueIdOrUrl) must be rejected by schema validation
	// as an isError result, not silently dispatched with a zero value.
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "sentry_get_issue", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call bad tool: %v", err)
	}
	if !bad.IsError {
		t.Fatalf("missing required arg should be an error, got: %+v", bad.Content)
	}
	if bt, ok := bad.Content[0].(*mcp.TextContent); !ok || !strings.Contains(bt.Text, "Invalid arguments") {
		t.Fatalf("want validation error, got: %+v", bad.Content)
	}

	// Wrong type (limit must be a number) must also be rejected.
	badType, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "sentry_search", Arguments: map[string]any{"limit": "lots"}})
	if err != nil {
		t.Fatalf("call bad-type tool: %v", err)
	}
	if !badType.IsError {
		t.Fatalf("wrong-typed arg should be an error, got: %+v", badType.Content)
	}
}
