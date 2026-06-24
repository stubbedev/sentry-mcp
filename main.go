package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Version is read from the embedded package.json (see tools.go) so every build
// path — go build, go install, nix, and the release binaries — reports the same
// version without any -ldflags wiring.
var Version = versionFromPkg()

const defaultProtocolVersion = "2025-06-18"

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[sentry-mcp] "+format+"\n", args...)
}

// ── JSON-RPC types ───────────────────────────────────────────────────────────

type rpcRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcIncoming is a superset that matches both inbound requests/notifications
// (have a method) and inbound responses to server-initiated requests (have
// result/error and no method).
type rpcIncoming struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

var sentry *SentryClient

func main() {
	if f := strings.ToLower(os.Getenv("SENTRY_MCP_FORMAT")); f == "json" || f == "toon" {
		defaultFormat = f
	}

	config := loadConfig()
	if config.Sentry != nil {
		sentry = NewSentryClient(config.Sentry.URL, config.Sentry.Token, config.Sentry.Org)
	} else {
		logf("No Sentry configuration found. Set sentry.{url,token,org} in ~/.sentry-mcp.json or SENTRY_URL/SENTRY_AUTH_TOKEN/SENTRY_ORG_SLUG env vars. Server will start with no tools registered.")
	}

	instructions := buildInstructions(config)

	// Graceful shutdown on signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	// Transport selection: HTTP when --http / SENTRY_MCP_HTTP_ADDR is set,
	// otherwise the default stdio transport.
	if addr := httpAddr(); addr != "" {
		if err := serveHTTP(addr, httpPath(), instructions); err != nil {
			logf("http server error: %v", err)
			os.Exit(1)
		}
		return
	}

	serveStdio(instructions)
}

// serveStdio runs the newline-delimited JSON-RPC loop over stdin/stdout. The
// connection is bidirectional: the server can issue requests to the client
// (e.g. roots/list), so each inbound request is handled in its own goroutine to
// keep the read loop free to deliver the client's responses.
func serveStdio(instructions string) {
	writer := bufio.NewWriter(os.Stdout)
	var writeMu sync.Mutex
	write := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writer.Write(b)
		writer.WriteByte('\n')
		return writer.Flush()
	}
	p := &peer{send: write, reg: newPendingRegistry()}

	var wg sync.WaitGroup
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			handleMessage(line, instructions, p, write, &wg)
		}
		if err != nil {
			if err != io.EOF {
				logf("read error: %v", err)
			}
			wg.Wait() // let in-flight handlers flush their responses
			return
		}
	}
}

// handleMessage parses one JSON-RPC message. Responses (client replies to
// server-initiated requests) are routed to the waiting caller; requests are
// dispatched in a goroutine so a handler that calls back to the client does not
// block the read loop. Notifications are handled inline.
func handleMessage(raw []byte, instructions string, p *peer, write func([]byte) error, wg *sync.WaitGroup) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return
	}
	var in rpcIncoming
	if err := json.Unmarshal(trimmed, &in); err != nil {
		logf("parse error: %v", err)
		return
	}
	if in.Method == "" && len(in.ID) > 0 {
		p.reg.deliver(idKey(in.ID), rpcReply{Result: in.Result, Error: in.Error})
		return
	}

	req := rpcRequest{Jsonrpc: in.Jsonrpc, ID: in.ID, Method: in.Method, Params: in.Params}
	if len(req.ID) == 0 {
		dispatch(&req, instructions, p) // notification — no response
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, _ := processRequest(&req, instructions, p)
		b, _ := json.Marshal(resp)
		write(b)
	}()
}

// processRequest dispatches a request and builds its response. The bool is
// false for notifications (caller should not write a response).
func processRequest(req *rpcRequest, instructions string, p *peer) (rpcResponse, bool) {
	if len(req.ID) == 0 {
		dispatch(req, instructions, p)
		return rpcResponse{}, false
	}
	result, rerr := dispatch(req, instructions, p)
	resp := rpcResponse{Jsonrpc: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	return resp, true
}

func dispatch(req *rpcRequest, instructions string, p *peer) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Roots *json.RawMessage `json:"roots"`
			} `json:"capabilities"`
		}
		json.Unmarshal(req.Params, &params)
		if p != nil && params.Capabilities.Roots != nil {
			p.markRootsCapable()
		}
		protocol := params.ProtocolVersion
		if protocol == "" {
			protocol = defaultProtocolVersion
		}
		return map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "sentry-mcp", "version": Version},
			"instructions":    instructions,
		}, nil

	case "notifications/initialized", "notifications/cancelled":
		return nil, nil

	case "notifications/roots/list_changed":
		if p != nil {
			p.invalidateRoots()
		}
		return nil, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": toolList()}, nil

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: codeInvalidRequest, Message: "invalid params: " + err.Error()}
		}
		return callTool(params.Name, params.Arguments, p)

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found: " + req.Method}
	}
}

// toolList returns the registered tool schemas, or an empty list when Sentry is
// not configured.
func toolList() []any {
	if sentry == nil {
		return []any{}
	}
	var tools []any
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		logf("tool schema parse error: %v", err)
		return []any{}
	}
	return tools
}

func requireSentry() (*SentryClient, *rpcError) {
	if sentry == nil {
		return nil, &rpcError{Code: codeInvalidRequest, Message: "Sentry is not configured. Set sentry.{url,token,org} in ~/.sentry-mcp.json or SENTRY_URL/SENTRY_AUTH_TOKEN/SENTRY_ORG_SLUG env vars."}
	}
	return sentry, nil
}

// callMu serializes tool calls. renderFormat and the sentry client are
// package globals set per-call; the stdio loop is inherently serial, but the
// HTTP transport may dispatch requests concurrently, so we serialize here to
// keep those globals race-free. Tool calls are I/O-bound on the Sentry API, so
// serialization costs little in practice.
var callMu sync.Mutex

// activePeer is the client connection for the in-flight tool call. Set under
// callMu so tool handlers can reach the client (e.g. to fetch workspace roots).
var activePeer *peer

// callCtx bounds the in-flight tool call. Set under callMu; consulted by Sentry
// requests and roots/list so no single tool call can hang the server (and, with
// it, every other call waiting on callMu).
var callCtx context.Context

// toolCallTimeout caps total wall-clock for one tool call, including any
// sequential Sentry requests and a roots/list round-trip.
const toolCallTimeout = 60 * time.Second

// callTool dispatches a tools/call. A returned *rpcError is a protocol-level
// error; a tool-execution error is returned as an isError tool result.
func callTool(name string, rawArgs map[string]any, p *peer) (any, *rpcError) {
	callMu.Lock()
	defer callMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), toolCallTimeout)
	defer cancel()
	callCtx = ctx
	activePeer = p
	defer func() { callCtx = nil; activePeer = nil }()

	args := normalizeArgs(rawArgs)
	renderFormat = pickFormat(args)

	client, rerr := requireSentry()
	if rerr != nil {
		return nil, rerr
	}

	result, err := runTool(client, name, args)
	if err != nil {
		if err == errUnknownTool {
			return nil, &rpcError{Code: codeMethodNotFound, Message: "Unknown tool: " + name}
		}
		return toolResult{Content: []contentBlock{{Type: "text", Text: "Error: " + err.Error()}}, IsError: true}, nil
	}
	return result, nil
}

var errUnknownTool = fmt.Errorf("unknown tool")

// defaultFormat is the process-wide output format, set once from
// SENTRY_MCP_FORMAT at startup (default "toon").
var defaultFormat = "toon"

// pickFormat resolves the output format for a call: per-call `format` arg wins,
// else the process default. Unknown values fall back to the default.
func pickFormat(args map[string]any) string {
	if f := strings.ToLower(argString(args, "format")); f == "json" || f == "toon" {
		return f
	}
	return defaultFormat
}

func runTool(c *SentryClient, name string, args map[string]any) (toolResult, error) {
	switch name {
	case "sentry_get_dev_context":
		return c.getDevContext()

	case "sentry_search":
		resource := argString(args, "resource")
		if resource == "" {
			resource = "issues"
		}
		switch resource {
		case "projects":
			return c.listProjects(argInt(args, "limit"), argString(args, "cursor"))
		case "teams":
			return c.listTeams(argInt(args, "limit"), argString(args, "cursor"))
		case "users":
			return c.listUsers(argString(args, "query"), argInt(args, "limit"), argString(args, "cursor"))
		default:
			projectSlug := argString(args, "projectSlug")
			if projectSlug == "" {
				return toolResult{}, fmt.Errorf("projectSlug (or project) is required for resource=issues.")
			}
			return c.listIssues(projectSlug, argString(args, "query"), argString(args, "status"), argInt(args, "limit"), argString(args, "cursor"))
		}

	case "sentry_get_issue":
		return c.getIssue(
			argString(args, "issueIdOrUrl"),
			argBool(args, "includeLatestEvent"),
			argStrSlice(args, "includeFields"),
			argStrSlice(args, "excludeFields"),
			argString(args, "grepPattern"),
			argIntPtr(args, "maxStackFrames"),
		)

	case "sentry_get_event":
		projectSlug := argString(args, "projectSlug")
		if projectSlug == "" {
			return toolResult{}, fmt.Errorf("projectSlug (or project) is required.")
		}
		return c.getEvent(projectSlug, argString(args, "eventId"), argInt(args, "limit"), argInt(args, "offset"), argString(args, "entryType"))

	case "sentry_mutate_issue":
		return c.mutateIssue(
			argString(args, "issueId"),
			argString(args, "status"), has(args, "status"),
			argString(args, "assignedTo"), has(args, "assignedTo"),
			argString(args, "comment"),
		)

	case "sentry_comment":
		action := argString(args, "action")
		if action == "" {
			action = "add"
		}
		issueId := argString(args, "issueId")
		commentId := argString(args, "commentId")
		body := argString(args, "body")
		switch action {
		case "update":
			if commentId == "" || body == "" {
				return toolResult{}, fmt.Errorf("update requires commentId and body.")
			}
			return c.editComment(issueId, commentId, body)
		case "delete":
			if commentId == "" {
				return toolResult{}, fmt.Errorf("delete requires commentId.")
			}
			return c.deleteComment(issueId, commentId)
		default:
			if body == "" {
				return toolResult{}, fmt.Errorf("add requires body.")
			}
			r, err := c.addComment(issueId, body)
			if err != nil {
				return toolResult{}, err
			}
			return textResult(r), nil
		}

	case "sentry_stack_frames":
		projectSlug := argString(args, "projectSlug")
		if projectSlug == "" {
			return toolResult{}, fmt.Errorf("projectSlug (or project) is required.")
		}
		return c.getStackFrames(projectSlug, argString(args, "eventId"), argBool(args, "inAppOnly"), argInt(args, "maxFrames"))

	case "sentry_check_dsym":
		projectSlug := argString(args, "projectSlug")
		if projectSlug == "" {
			return toolResult{}, fmt.Errorf("projectSlug (or project) is required.")
		}
		return c.checkDsymStatus(projectSlug, argString(args, "eventId"))

	case "sentry_raw_api":
		var params map[string]any
		if p, ok := args["params"].(map[string]any); ok {
			params = p
		}
		return c.rawApi(
			argString(args, "endpoint"),
			argString(args, "method"),
			params,
			args["body"],
			argString(args, "grepPattern"),
			argInt(args, "maxChars"),
			argInt(args, "charOffset"),
		)

	default:
		return toolResult{}, errUnknownTool
	}
}

// normalizeArgs maps the `project` alias onto `projectSlug`.
func normalizeArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	if _, ok := args["projectSlug"].(string); !ok {
		if p, ok := args["project"].(string); ok {
			args["projectSlug"] = p
		}
	}
	return args
}

// ── Argument helpers ─────────────────────────────────────────────────────────

func has(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func argString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func argBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func argInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case int:
		return v
	case string:
		var i int
		fmt.Sscanf(v, "%d", &i)
		return i
	}
	return 0
}

func argIntPtr(m map[string]any, k string) *int {
	if !has(m, k) {
		return nil
	}
	i := argInt(m, k)
	return &i
}

func argStrSlice(m map[string]any, k string) []string {
	arr, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ── Instructions ─────────────────────────────────────────────────────────────

func buildInstructions(config Config) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	w("# sentry-mcp")
	w("")
	w("Self-hosted Sentry tooling for triaging issues, inspecting events, and managing assignments. Prefer these tools over shelling out to `curl` or guessing endpoint paths.")
	w("")

	if sentry == nil || config.Sentry == nil {
		w("## Configured instance")
		b.WriteString("- (not configured — set SENTRY_URL, SENTRY_AUTH_TOKEN, SENTRY_ORG_SLUG)")
		return b.String()
	}

	me := sentry.whoami()

	w("## Configured instance")
	w("- URL:  " + config.Sentry.URL)
	w("- Org:  " + config.Sentry.Org)
	if me != nil {
		ident := me.ident()
		if ident == "" {
			ident = "?"
		}
		suffix := ""
		if me.email != "" && me.email != me.username {
			suffix = " <" + me.email + ">"
		}
		w("- You:  " + ident + suffix)
	}

	if projects := sentry.fetchProjects(20); len(projects) > 0 {
		w("")
		w(fmt.Sprintf("## Projects (top %d)", len(projects)))
		for _, p := range projects {
			slug := p.slug
			if slug == "" {
				slug = "?"
			}
			line := "- " + slug
			if p.platform != "" {
				line += " (" + p.platform + ")"
			}
			w(line)
		}
	}

	w("")
	w("## Use these tools — do NOT guess")
	w("- \"what am I working on / show me the context\" → call `sentry_get_dev_context` first.")
	w("- Looking up a person's username (for `assignedTo`) → ALWAYS use `sentry_search resource=users`. NEVER guess from git authors or email prefixes — the wrong username silently breaks `sentry_mutate_issue`.")
	w("- Reading an issue → `sentry_get_issue`. Stack traces only → `sentry_stack_frames`. Full event → `sentry_get_event`.")
	w("- Mutating an issue → `sentry_mutate_issue`. Comments → `sentry_comment`.")
	w("- Missing iOS/macOS/Android symbols → `sentry_check_dsym` returns the UUIDs to upload.")
	w("- Anything else → `sentry_raw_api`. Always pass `grepPattern` or `maxChars`/`charOffset` for endpoints that may return large events.")
	w("")
	b.WriteString("IMPORTANT: do NOT resolve, ignore, or reassign issues without an explicit user instruction. Read tools are safe; mutation tools are not.")

	return b.String()
}
