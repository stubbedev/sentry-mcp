package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			handleLine(line, instructions, writer)
			writer.Flush()
		}
		if err != nil {
			if err == io.EOF {
				return
			}
			logf("read error: %v", err)
			return
		}
	}
}

func handleLine(line []byte, instructions string, writer *bufio.Writer) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return
	}
	var req rpcRequest
	if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
		// Cannot parse — without an id we cannot respond meaningfully.
		logf("parse error: %v", err)
		return
	}
	isNotification := len(req.ID) == 0

	result, rerr := dispatch(&req, instructions)

	if isNotification {
		return // Notifications get no response.
	}
	resp := rpcResponse{Jsonrpc: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	out, _ := json.Marshal(resp)
	writer.Write(out)
	writer.WriteByte('\n')
}

func dispatch(req *rpcRequest, instructions string) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		protocol := p.ProtocolVersion
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

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": toolList()}, nil

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidRequest, Message: "invalid params: " + err.Error()}
		}
		return callTool(p.Name, p.Arguments)

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

// callTool dispatches a tools/call. A returned *rpcError is a protocol-level
// error; a tool-execution error is returned as an isError tool result.
func callTool(name string, rawArgs map[string]any) (any, *rpcError) {
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
