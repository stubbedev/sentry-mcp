package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mcpRoot is a workspace root exposed by the MCP client (the "roots" feature).
// It carries the repo/working-tree location the harness wants the server to
// operate in — the input a shell-calling tool would need.
type mcpRoot struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
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

// rpcReply is a client's response to a server-initiated request.
type rpcReply struct {
	Result json.RawMessage
	Error  *rpcError
}

// idKey normalizes a JSON-RPC id (string or number) to a registry key.
func idKey(id json.RawMessage) string {
	var s string
	if err := json.Unmarshal(id, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(id))
}

// pendingRegistry correlates server→client request ids with waiting callers.
type pendingRegistry struct {
	mu sync.Mutex
	m  map[string]chan rpcReply
}

func newPendingRegistry() *pendingRegistry {
	return &pendingRegistry{m: map[string]chan rpcReply{}}
}

func (r *pendingRegistry) register(id string) chan rpcReply {
	ch := make(chan rpcReply, 1)
	r.mu.Lock()
	r.m[id] = ch
	r.mu.Unlock()
	return ch
}

func (r *pendingRegistry) unregister(id string) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

func (r *pendingRegistry) deliver(id string, reply rpcReply) bool {
	r.mu.Lock()
	ch, ok := r.m[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- reply:
	default:
	}
	return true
}

// peer is the client side of a connection. It can issue server→client requests
// (e.g. roots/list) and caches the resulting workspace roots.
type peer struct {
	send func([]byte) error // deliver one framed JSON message to the client; nil if unavailable
	reg  *pendingRegistry
	idc  uint64

	mu           sync.Mutex
	rootsCapable bool      // client advertised the roots capability at initialize
	roots        []mcpRoot // cached roots
	rootsFetched bool      // roots resolved (via header or roots/list)
	headerRoots  bool      // roots came from a request header (authoritative)
}

// call sends a server→client JSON-RPC request and waits for the response.
func (p *peer) call(method string, params any) (json.RawMessage, *rpcError) {
	if p == nil || p.send == nil {
		return nil, &rpcError{Code: codeInternalError, Message: "no client channel for " + method}
	}
	id := fmt.Sprintf("srv-%d", atomic.AddUint64(&p.idc, 1))
	ch := p.reg.register(id)
	defer p.reg.unregister(id)

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if err := p.send(b); err != nil {
		return nil, &rpcError{Code: codeInternalError, Message: err.Error()}
	}

	// Bound the wait: the call's deadline (callCtx), a fixed cap, or the reply.
	var done <-chan struct{}
	if callCtx != nil {
		done = callCtx.Done()
	}
	select {
	case reply := <-ch:
		return reply.Result, reply.Error
	case <-done:
		return nil, &rpcError{Code: codeInternalError, Message: method + " cancelled"}
	case <-time.After(10 * time.Second):
		return nil, &rpcError{Code: codeInternalError, Message: "timeout waiting for " + method}
	}
}

// setHeaderRoots records roots supplied via a request header. These take
// precedence over roots/list and are never overridden by list_changed.
func (p *peer) setHeaderRoots(roots []mcpRoot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roots = roots
	p.rootsFetched = true
	p.headerRoots = true
}

// invalidateRoots clears cached roots so the next listRoots refetches. A no-op
// when roots were pinned via a header.
func (p *peer) invalidateRoots() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.headerRoots {
		return
	}
	p.rootsFetched = false
	p.roots = nil
}

// markRootsCapable records that the client advertised the roots capability.
func (p *peer) markRootsCapable() {
	p.mu.Lock()
	p.rootsCapable = true
	p.mu.Unlock()
}

// listRoots returns the client's workspace roots. Header-provided roots are
// returned immediately; otherwise, when the client supports roots, they are
// fetched once via roots/list and cached.
func (p *peer) listRoots() []mcpRoot {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.rootsFetched || !p.rootsCapable || p.send == nil {
		roots := p.roots
		p.mu.Unlock()
		return roots
	}
	p.mu.Unlock()

	reply, rerr := p.call("roots/list", nil)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rootsFetched { // a header set may have won the race
		return p.roots
	}
	p.rootsFetched = true
	if rerr != nil {
		logf("roots/list failed: %s", rerr.Message)
		return nil
	}
	var out struct {
		Roots []mcpRoot `json:"roots"`
	}
	if err := json.Unmarshal(reply, &out); err != nil {
		logf("roots/list parse error: %v", err)
		return nil
	}
	p.roots = out.Roots
	return p.roots
}
