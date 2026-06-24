package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Default HTTP bind address and endpoint path. The default host is loopback so
// that, behind a reverse proxy, the server is not inadvertently exposed; set an
// explicit host (e.g. 0.0.0.0:8765) to listen on all interfaces.
const (
	defaultHTTPAddr = "127.0.0.1:8765"
	defaultHTTPPath = "/mcp"

	sessionHeader   = "Mcp-Session-Id"
	maxRequestBytes = 4 << 20          // 4 MiB per request body
	sessionTTL      = 30 * time.Minute // idle sessions are reaped after this
)

// rootHeaders are the request headers a proxy/harness may set to hand the
// server the workspace root(s) without the MCP roots round-trip. Values are
// file:// URIs or plain paths; multiple roots may be comma-separated.
var rootHeaders = []string{"X-Mcp-Roots", "X-Mcp-Root", "Mcp-Roots", "Mcp-Root"}

var errNoStream = errors.New("client has no open SSE stream to receive server messages")

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

// ── Sessions ─────────────────────────────────────────────────────────────────

// httpSession is one stateful client connection. outbound carries server→client
// messages, drained by the GET SSE stream.
type httpSession struct {
	id       string
	peer     *peer
	outbound chan []byte
	lastSeen atomicTime
	streams  atomic.Int32 // number of open GET SSE streams
}

func (s *httpSession) touch() { s.lastSeen.set(time.Now()) }

type sessionStore struct {
	mu sync.Mutex
	m  map[string]*httpSession
}

func newSessionStore() *sessionStore { return &sessionStore{m: map[string]*httpSession{}} }

func (st *sessionStore) create() *httpSession {
	s := &httpSession{id: randomID(), outbound: make(chan []byte, 32)}
	s.peer = &peer{reg: newPendingRegistry()}
	s.peer.send = func(b []byte) error {
		if s.streams.Load() == 0 {
			return errNoStream // no SSE stream to deliver on — fail fast
		}
		select {
		case s.outbound <- b:
			return nil
		case <-time.After(5 * time.Second):
			return errNoStream
		}
	}
	s.touch()
	st.mu.Lock()
	st.m[s.id] = s
	st.mu.Unlock()
	return s
}

func (st *sessionStore) get(id string) *httpSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.m[id]
	if s != nil {
		s.touch()
	}
	return s
}

func (st *sessionStore) remove(id string) {
	st.mu.Lock()
	delete(st.m, id)
	st.mu.Unlock()
}

// reap drops sessions idle longer than sessionTTL.
func (st *sessionStore) reap() {
	cutoff := time.Now().Add(-sessionTTL)
	st.mu.Lock()
	for id, s := range st.m {
		if s.lastSeen.get().Before(cutoff) {
			delete(st.m, id)
		}
	}
	st.mu.Unlock()
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// atomicTime is a tiny mutex-guarded time holder (Time is not atomic).
type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }

// ── Server ───────────────────────────────────────────────────────────────────

// serveHTTP runs the MCP server over the Streamable HTTP transport on a single
// endpoint. Clients POST JSON-RPC requests (single or batched) and receive JSON
// responses; an optional GET opens an SSE stream carrying server→client
// requests (used for roots/list). Sessions are tracked via the Mcp-Session-Id
// header issued at initialize.
func serveHTTP(addr, path, instructions string) error {
	store := newSessionStore()

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		handleMCP(w, r, instructions, store)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// Idle-session reaper.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				store.reap()
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	logf("listening on http://%s%s (MCP Streamable HTTP)", addr, path)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func handleMCP(w http.ResponseWriter, r *http.Request, instructions string, store *sessionStore) {
	switch r.Method {
	case http.MethodPost:
		handlePost(w, r, instructions, store)
	case http.MethodGet:
		handleGet(w, r, store)
	case http.MethodDelete:
		store.remove(r.Header.Get(sessionHeader))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handlePost processes one JSON-RPC request, batch, or client response.
func handlePost(w http.ResponseWriter, r *http.Request, instructions string, store *sessionStore) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeRPC(w, rpcResponse{Jsonrpc: "2.0", Error: &rpcError{Code: codeInvalidRequest, Message: "empty request"}})
		return
	}

	// Resolve the session: initialize creates one; everything else requires the
	// session header issued at initialize.
	var s *httpSession
	if isInitialize(trimmed) {
		s = store.create()
		w.Header().Set(sessionHeader, s.id)
	} else {
		s = store.get(r.Header.Get(sessionHeader))
		if s == nil {
			writeHTTPError(w, http.StatusNotFound, "unknown or missing "+sessionHeader+"; send initialize first")
			return
		}
	}

	// A proxy/harness may pin roots via headers — authoritative, no round-trip.
	if roots := parseRootHeaders(r.Header); roots != nil {
		s.peer.setHeaderRoots(roots)
	}

	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			writeRPC(w, rpcResponse{Jsonrpc: "2.0", Error: &rpcError{Code: codeInvalidRequest, Message: "invalid JSON batch: " + err.Error()}})
			return
		}
		var responses []rpcResponse
		for _, raw := range batch {
			if resp, ok := processIncoming(raw, instructions, s.peer); ok {
				responses = append(responses, resp)
			}
		}
		if len(responses) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, responses)
		return
	}

	resp, ok := processIncoming(trimmed, instructions, s.peer)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, resp)
}

// handleGet opens the server→client SSE stream for a session.
func handleGet(w http.ResponseWriter, r *http.Request, store *sessionStore) {
	s := store.get(r.Header.Get(sessionHeader))
	if s == nil {
		writeHTTPError(w, http.StatusNotFound, "unknown or missing "+sessionHeader)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeHTTPError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	s.streams.Add(1)
	defer s.streams.Add(-1)

	ctx := r.Context()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-s.outbound:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// processIncoming parses and handles one message. Client responses (to
// server-initiated requests) are routed to waiters and produce no reply (ok
// false); requests are dispatched; notifications produce no reply.
func processIncoming(raw json.RawMessage, instructions string, p *peer) (rpcResponse, bool) {
	var in rpcIncoming
	if err := json.Unmarshal(raw, &in); err != nil {
		return rpcResponse{Jsonrpc: "2.0", Error: &rpcError{Code: codeInvalidRequest, Message: "parse error: " + err.Error()}}, true
	}
	if in.Method == "" && len(in.ID) > 0 {
		p.reg.deliver(idKey(in.ID), rpcReply{Result: in.Result, Error: in.Error})
		return rpcResponse{}, false
	}
	req := rpcRequest{Jsonrpc: in.Jsonrpc, ID: in.ID, Method: in.Method, Params: in.Params}
	return processRequest(&req, instructions, p)
}

func isInitialize(trimmed []byte) bool {
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var m struct {
		Method string `json:"method"`
	}
	json.Unmarshal(trimmed, &m)
	return m.Method == "initialize"
}

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

func writeRPC(w http.ResponseWriter, resp rpcResponse) { writeJSON(w, http.StatusOK, resp) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeHTTPError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
