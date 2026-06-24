package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// do drives one request through the HTTP handler against the given store.
func do(store *sessionStore, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handleMCP(w, r, "test-instructions", store)
	return w
}

func TestHTTPSessionLifecycle(t *testing.T) {
	store := newSessionStore()

	// initialize issues a session id.
	w := do(store, "POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"roots":{}}}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: got %d", w.Code)
	}
	sid := w.Header().Get(sessionHeader)
	if sid == "" {
		t.Fatal("initialize did not return a session id")
	}

	// Non-initialize without a session id is rejected.
	if w := do(store, "POST", `{"jsonrpc":"2.0","id":2,"method":"ping"}`, nil); w.Code != http.StatusNotFound {
		t.Fatalf("ping without session: got %d, want 404", w.Code)
	}

	// With the session id it succeeds.
	w = do(store, "POST", `{"jsonrpc":"2.0","id":2,"method":"ping"}`, map[string]string{sessionHeader: sid})
	if w.Code != http.StatusOK {
		t.Fatalf("ping with session: got %d", w.Code)
	}

	// Notifications return 202 with no body.
	if w := do(store, "POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, map[string]string{sessionHeader: sid}); w.Code != http.StatusAccepted {
		t.Fatalf("notification: got %d, want 202", w.Code)
	}

	// DELETE ends the session.
	if w := do(store, "DELETE", "", map[string]string{sessionHeader: sid}); w.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", w.Code)
	}
	if store.get(sid) != nil {
		t.Fatal("session still present after delete")
	}
}

func TestHTTPHeaderRoots(t *testing.T) {
	store := newSessionStore()
	w := do(store, "POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	sid := w.Header().Get(sessionHeader)

	// A request carrying a root header pins it on the session peer.
	do(store, "POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		map[string]string{sessionHeader: sid, "X-Mcp-Root": "file:///srv/x"})

	s := store.get(sid)
	roots := s.peer.listRoots()
	if len(roots) != 1 || roots[0].path() != "/srv/x" {
		t.Fatalf("header root not pinned: %+v", roots)
	}
}

// TestHTTPRootsListOverStream drives the full server→client roots/list round
// trip: the server enqueues the request on the session's outbound channel
// (as the SSE stream would drain it) and the simulated client replies via a
// POST carrying a JSON-RPC response.
func TestHTTPRootsListOverStream(t *testing.T) {
	store := newSessionStore()
	s := store.create()
	s.streams.Add(1) // pretend an SSE stream is open
	s.peer.markRootsCapable()

	go func() {
		b := <-s.outbound
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Method != "roots/list" {
			return
		}
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"roots":[{"uri":"file:///srv/r","name":"r"}]}}`, m.ID)
		processIncoming(json.RawMessage(resp), "test", s.peer)
	}()

	roots := s.peer.listRoots()
	if len(roots) != 1 || roots[0].URI != "file:///srv/r" || roots[0].Name != "r" {
		t.Fatalf("roots/list round trip failed: %+v", roots)
	}

	// Cached: a second call returns the same without another round trip.
	if again := s.peer.listRoots(); len(again) != 1 {
		t.Fatalf("roots not cached: %+v", again)
	}
}

// TestHTTPRootsListNoStreamFastFails ensures a roots-capable client with no SSE
// stream open does not block: listRoots returns promptly with no roots.
func TestHTTPRootsListNoStreamFastFails(t *testing.T) {
	store := newSessionStore()
	s := store.create()
	s.peer.markRootsCapable() // capable, but streams == 0

	done := make(chan []mcpRoot, 1)
	go func() { done <- s.peer.listRoots() }()
	select {
	case roots := <-done:
		if len(roots) != 0 {
			t.Fatalf("expected no roots, got %+v", roots)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listRoots blocked despite no open stream")
	}
}
