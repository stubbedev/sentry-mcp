package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is read from the embedded package.json (see tools.go) so every build
// path — go build, go install, nix, and the release binaries — reports the same
// version without any -ldflags wiring.
var Version = versionFromPkg()

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[sentry-mcp] "+format+"\n", args...)
}

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

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "sentry-mcp", Version: Version},
		&mcp.ServerOptions{Instructions: buildInstructions(config)},
	)
	registerTools(srv)

	// Graceful shutdown: ctx is cancelled on SIGINT/SIGTERM, which stops the
	// stdio Run loop and triggers HTTP server shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Transport selection: HTTP when --http / SENTRY_MCP_HTTP_ADDR is set,
	// otherwise the default stdio transport.
	if addr := httpAddr(); addr != "" {
		if err := serveHTTP(ctx, srv, addr, httpPath()); err != nil {
			logf("http server error: %v", err)
			os.Exit(1)
		}
		return
	}

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		logf("stdio server error: %v", err)
		os.Exit(1)
	}
}

// validators holds the resolved input schema per tool name, used to validate
// arguments before dispatch. Populated in registerTools.
var validators = map[string]*jsonschema.Resolved{}

// registerTools adds every tool in the embedded tools.json to the server,
// sharing one dispatcher, and compiles each tool's input schema for argument
// validation. When Sentry is not configured no tools are registered, so the
// client sees an empty tool list.
func registerTools(srv *mcp.Server) {
	if sentry == nil {
		return
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		logf("tool schema parse error: %v", err)
		return
	}
	for _, t := range tools {
		srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}, toolHandler)

		var schema jsonschema.Schema
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			logf("tool %s: schema parse error: %v", t.Name, err)
			continue
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			logf("tool %s: schema resolve error: %v", t.Name, err)
			continue
		}
		validators[t.Name] = resolved
	}
}

func requireSentry() (*SentryClient, error) {
	if sentry == nil {
		return nil, fmt.Errorf("Sentry is not configured. Set sentry.{url,token,org} in ~/.sentry-mcp.json or SENTRY_URL/SENTRY_AUTH_TOKEN/SENTRY_ORG_SLUG env vars.")
	}
	return sentry, nil
}

// toolCallTimeout caps total wall-clock for one tool call, including any
// sequential Sentry requests and a roots/list round-trip.
const toolCallTimeout = 60 * time.Second

var errUnknownTool = fmt.Errorf("unknown tool")

// toolHandler dispatches every tools/call. Arguments are validated against the
// tool's JSON schema before dispatch; an execution or validation error is
// returned as an isError tool result so the model can self-correct, while an
// unknown-tool or unconfigured error is a protocol-level error. The per-call
// ctx carries the deadline and the output format, so concurrent calls share no
// mutable package state.
func toolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()

	rawArgs := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &rawArgs); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if v := validators[req.Params.Name]; v != nil {
		if err := v.Validate(rawArgs); err != nil {
			return toolErr("Invalid arguments: " + err.Error()), nil
		}
	}

	args := normalizeArgs(rawArgs)
	ctx = ctxWithFormat(ctx, pickFormat(args))

	client, err := requireSentry()
	if err != nil {
		return nil, err
	}

	result, err := runTool(ctx, req, client, req.Params.Name, args)
	if err != nil {
		if err == errUnknownTool {
			return nil, fmt.Errorf("unknown tool: %s", req.Params.Name)
		}
		return toolErr("Error: " + err.Error()), nil
	}
	return toCallResult(result), nil
}

// toolErr builds an isError tool result carrying a message the model can read.
func toolErr(msg string) *mcp.CallToolResult {
	return toCallResult(toolResult{Content: []contentBlock{{Type: "text", Text: msg}}, IsError: true})
}

// toCallResult converts an internal toolResult into the SDK's CallToolResult.
func toCallResult(r toolResult) *mcp.CallToolResult {
	out := &mcp.CallToolResult{IsError: r.IsError}
	for _, c := range r.Content {
		out.Content = append(out.Content, &mcp.TextContent{Text: c.Text})
	}
	return out
}

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

func runTool(ctx context.Context, req *mcp.CallToolRequest, c *SentryClient, name string, args map[string]any) (toolResult, error) {
	switch name {
	case "sentry_get_dev_context":
		return c.getDevContext(ctx, req)

	case "sentry_search":
		resource := argString(args, "resource")
		if resource == "" {
			resource = "issues"
		}
		switch resource {
		case "projects":
			return c.listProjects(ctx, argInt(args, "limit"), argString(args, "cursor"))
		case "teams":
			return c.listTeams(ctx, argInt(args, "limit"), argString(args, "cursor"))
		case "users":
			return c.listUsers(ctx, argString(args, "query"), argInt(args, "limit"), argString(args, "cursor"))
		default:
			projectSlug := argString(args, "projectSlug")
			if projectSlug == "" {
				return toolResult{}, fmt.Errorf("projectSlug (or project) is required for resource=issues.")
			}
			return c.listIssues(ctx, projectSlug, argString(args, "query"), argString(args, "status"), argInt(args, "limit"), argString(args, "cursor"))
		}

	case "sentry_get_issue":
		return c.getIssue(ctx,
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
		return c.getEvent(ctx, projectSlug, argString(args, "eventId"), argInt(args, "limit"), argInt(args, "offset"), argString(args, "entryType"))

	case "sentry_mutate_issue":
		return c.mutateIssue(ctx,
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
			return c.editComment(ctx, issueId, commentId, body)
		case "delete":
			if commentId == "" {
				return toolResult{}, fmt.Errorf("delete requires commentId.")
			}
			return c.deleteComment(ctx, issueId, commentId)
		default:
			if body == "" {
				return toolResult{}, fmt.Errorf("add requires body.")
			}
			r, err := c.addComment(ctx, issueId, body)
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
		return c.getStackFrames(ctx, projectSlug, argString(args, "eventId"), argBool(args, "inAppOnly"), argInt(args, "maxFrames"))

	case "sentry_check_dsym":
		projectSlug := argString(args, "projectSlug")
		if projectSlug == "" {
			return toolResult{}, fmt.Errorf("projectSlug (or project) is required.")
		}
		return c.checkDsymStatus(ctx, projectSlug, argString(args, "eventId"))

	case "sentry_raw_api":
		var params map[string]any
		if p, ok := args["params"].(map[string]any); ok {
			params = p
		}
		return c.rawApi(ctx,
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

	me := sentry.whoami(context.Background())

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

	if projects := sentry.fetchProjects(context.Background(), 20); len(projects) > 0 {
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
