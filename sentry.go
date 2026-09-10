package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	toon "github.com/toon-format/toon-go"
)

// ── Tool result types ───────────────────────────────────────────────────────

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

func textResult(t string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: t}}}
}

// formatKey carries the per-call output format ("toon" or "json") on the
// request context, so concurrent tool calls render independently without any
// shared package state. ponytail: context value, not a param threaded through
// every render site — set once in the handler, read in renderString.
type formatKey struct{}

func ctxWithFormat(ctx context.Context, format string) context.Context {
	return context.WithValue(ctx, formatKey{}, format)
}

func formatFromCtx(ctx context.Context) string {
	if f, _ := ctx.Value(formatKey{}).(string); f == "json" {
		return "json"
	}
	return "toon"
}

// renderString serializes v in the call's output format. TOON is the default;
// on any encoding error it falls back to pretty JSON.
func renderString(ctx context.Context, v any) string {
	if formatFromCtx(ctx) == "json" {
		return marshalIndent(v)
	}
	s, err := toon.MarshalString(v)
	if err != nil {
		return marshalIndent(v)
	}
	return s
}

// jsonResult renders structured data as a text tool result.
func jsonResult(ctx context.Context, v any) toolResult {
	return textResult(renderString(ctx, v))
}

// marshalIndent pretty-prints JSON with 2-space indent and without HTML
// escaping, matching JSON.stringify(obj, null, 2).
func marshalIndent(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// ── Error formatting ─────────────────────────────────────────────────────────

func parseSentryErrorDetails(errText string) string {
	trimmed := strings.TrimSpace(errText)
	if trimmed == "" {
		return ""
	}
	var parsed struct {
		Detail  string          `json:"detail"`
		Message string          `json:"message"`
		Errors  json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		if parsed.Detail != "" {
			return parsed.Detail
		}
		if parsed.Message != "" {
			return parsed.Message
		}
		if len(parsed.Errors) > 0 && string(parsed.Errors) != "null" {
			return string(parsed.Errors)
		}
	}
	if len(trimmed) > 500 {
		return trimmed[:500] + "..."
	}
	return trimmed
}

func formatSentryError(status int, method, path, details string) string {
	prefix := fmt.Sprintf("Sentry %d %s %s", status, method, path)
	switch status {
	case 400:
		return strings.TrimSpace(fmt.Sprintf("%s. Invalid request. %s", prefix, details))
	case 401:
		return prefix + ". Authentication failed. Check SENTRY_AUTH_TOKEN."
	case 403:
		return prefix + ". Permission denied. Check token scopes (need org:read, project:read, event:read, etc.)."
	case 404:
		return prefix + ". Resource not found. Verify org slug, project slug, issue/event ID."
	}
	if details != "" {
		return prefix + ". " + details
	}
	return prefix
}

// ── Generic helpers ──────────────────────────────────────────────────────────

// extractIssueId pulls an issue ID out of a numeric string or a Sentry issue URL.
func extractIssueId(input string) string {
	if strings.Contains(input, "://") {
		if u, err := url.Parse(input); err == nil {
			parts := splitNonEmpty(u.Path, "/")
			for i, p := range parts {
				if p == "issues" && i+1 < len(parts) {
					if isAllDigits(parts[i+1]) {
						return parts[i+1]
					}
				}
			}
		}
		return ""
	}
	if isAllDigits(input) {
		return input
	}
	return ""
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// filterFields keeps or drops object fields based on include/exclude lists,
// with dot notation for nested fields. Include takes precedence over exclude.
func filterFields(obj any, include, exclude []string) any {
	switch v := obj.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = filterFields(item, include, exclude)
		}
		return out
	case map[string]any:
		result := map[string]any{}
		if len(include) > 0 {
			for _, field := range include {
				if parent, rest, ok := cutField(field); ok {
					if val, exists := v[parent]; exists {
						result[parent] = filterFields(val, []string{rest}, nil)
					}
				} else if val, exists := v[field]; exists {
					result[field] = val
				}
			}
			return result
		}
		for k, val := range v {
			result[k] = val
		}
		for _, field := range exclude {
			if parent, rest, ok := cutField(field); ok {
				if val, exists := result[parent]; exists {
					result[parent] = filterFields(val, nil, []string{rest})
				}
			} else {
				delete(result, field)
			}
		}
		return result
	default:
		return obj
	}
}

func cutField(field string) (parent, rest string, ok bool) {
	idx := strings.IndexByte(field, '.')
	if idx < 0 {
		return "", "", false
	}
	return field[:idx], field[idx+1:], true
}

// grepRendered filters a response to the lines matching pattern, plus one line
// of context either side.
//
// It greps the text the caller actually receives — rendered in the call's
// output format — rather than a pretty-printed JSON form. Grepping JSON while
// returning TOON meant a pattern had to be written against a shape the response
// never showed: `"function":` matched nothing a reader could see. For a TOON
// table the header line is always kept, since without it the matched rows have
// no column names.
func grepRendered(ctx context.Context, data any, pattern string) (string, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return "", fmt.Errorf("invalid grepPattern: %v", err)
	}
	lines := strings.Split(renderString(ctx, data), "\n")
	keep := make([]bool, len(lines))
	matches := 0
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		matches++
		if i > 0 {
			keep[i-1] = true
		}
		keep[i] = true
		if i+1 < len(lines) {
			keep[i+1] = true
		}
	}
	if matches == 0 {
		return fmt.Sprintf("No lines match %q (searched %d lines of output).", pattern, len(lines)), nil
	}
	keep[0] = true // the table header, or the first field of an object
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if keep[i] {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), nil
}

func asString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// ── Sentry client ────────────────────────────────────────────────────────────

type SentryClient struct {
	baseURL string
	OrgSlug string
	token   string
	http    *http.Client
}

const (
	// maxAttempts bounds how many times one Sentry request is sent.
	maxAttempts = 3
	// retryBaseWait is the first backoff, doubling per attempt unless the
	// server asks for longer via Retry-After.
	retryBaseWait = 250 * time.Millisecond
	// maxRetryWait caps a single wait, so an extravagant Retry-After is
	// declined rather than obeyed inside a 60s tool call.
	maxRetryWait = 10 * time.Second
)

func NewSentryClient(baseURL, token, orgSlug string) *SentryClient {
	return &SentryClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		OrgSlug: orgSlug,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type apiResponse struct {
	data       any
	linkHeader string
	status     int
}

// request performs one Sentry API call, retrying transient failures within
// whatever deadline the caller's context carries.
func (c *SentryClient) request(ctx context.Context, method, path string, params map[string]any, body any) (apiResponse, error) {
	cleanPath := path
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}
	qs := ""
	if len(params) > 0 {
		vals := url.Values{}
		for k, v := range params {
			if v == nil {
				continue
			}
			vals.Add(k, toQueryString(v))
		}
		if enc := vals.Encode(); enc != "" {
			qs = "?" + enc
		}
	}
	fullURL := c.baseURL + "/api/0" + cleanPath + qs

	// Marshalled once but wrapped in a fresh reader per attempt, since a
	// consumed body cannot be replayed.
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, err
		}
		bodyBytes = b
	}

	if ctx == nil {
		ctx = context.Background()
	}

	var lastErr error
	wait := time.Duration(0)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && !waitBeforeRetry(ctx, wait) {
			break
		}
		resp, retryAfter, retryable, err := c.attempt(ctx, method, fullURL, cleanPath, bodyBytes)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable {
			return apiResponse{}, err
		}
		wait = retryBaseWait << attempt
		if retryAfter > wait {
			wait = retryAfter
		}
	}
	return apiResponse{}, lastErr
}

// attempt makes a single HTTP call. retryable reports whether repeating it is
// both safe and worthwhile; retryAfter carries the server's own backoff ask.
func (c *SentryClient) attempt(ctx context.Context, method, fullURL, cleanPath string, bodyBytes []byte) (resp apiResponse, retryAfter time.Duration, retryable bool, err error) {
	var reqBody io.Reader
	if bodyBytes != nil {
		reqBody = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return apiResponse{}, 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		// No response arrived, so whether the request was applied is unknown:
		// only methods that are safe to repeat may be retried.
		return apiResponse{}, 0, ctx.Err() == nil && repeatableMethod(method), err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		errText, _ := io.ReadAll(res.Body)
		wait := parseRetryAfter(res.Header.Get("Retry-After"))
		return apiResponse{}, wait, retryableStatus(res.StatusCode, method),
			fmt.Errorf("%s", formatSentryError(res.StatusCode, method, cleanPath, parseSentryErrorDetails(string(errText))))
	}

	out := apiResponse{linkHeader: res.Header.Get("link"), status: res.StatusCode}
	if res.StatusCode == 204 {
		return out, 0, false, nil
	}
	dec := json.NewDecoder(res.Body)
	if err := dec.Decode(&out.data); err != nil && err != io.EOF {
		return apiResponse{}, 0, false, err
	}
	return out, 0, false, nil
}

// repeatableMethod reports whether re-sending a request is free of side
// effects. POST is excluded: a retried comment would post twice.
func repeatableMethod(method string) bool {
	return method == "GET" || method == "PUT" || method == "DELETE"
}

// retryableStatus decides whether a failure status is worth another attempt.
// 429 means the request was rejected before it did anything, so it is safe to
// repeat for any method; a gateway error may already have been applied, so
// those are retried only for repeatable methods.
func retryableStatus(status int, method string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return repeatableMethod(method)
	}
	return false
}

// parseRetryAfter reads a Retry-After header in either of its forms: delay
// seconds, or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// waitBeforeRetry sleeps for d, reporting whether the wait completed. It
// refuses a wait that would outlast the caller's deadline, so a retry never
// eats the whole tool-call budget just to fail at the end of it.
func waitBeforeRetry(ctx context.Context, d time.Duration) bool {
	if d > maxRetryWait {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok && time.Now().Add(d).After(deadline) {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func toQueryString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		// Render integers without a trailing .0
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// parseNextCursor extracts the `next` cursor from Sentry's Link header when
// results="true".
func (c *SentryClient) parseNextCursor(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	for _, seg := range strings.Split(linkHeader, ",") {
		if !strings.Contains(seg, `rel="next"`) {
			continue
		}
		if !strings.Contains(seg, `results="true"`) {
			return ""
		}
		m := regexp.MustCompile(`cursor="([^"]+)"`).FindStringSubmatch(seg)
		if len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func clampLimit(limit, def int) int {
	if limit <= 0 {
		limit = def
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	return limit
}

func toArray(v any) []any {
	a, _ := v.([]any)
	return a
}

func toObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// ── Discovery ────────────────────────────────────────────────────────────────

type identity struct {
	id, username, email, name string
}

func (i *identity) ident() string {
	for _, s := range []string{i.username, i.email, i.name} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (c *SentryClient) whoami(ctx context.Context) *identity {
	// /auth/ works for both user PATs and org auth tokens; /users/me/ rejects org tokens.
	resp, err := c.request(ctx, "GET", "/auth/", nil, nil)
	if err != nil {
		return nil
	}
	data := toObject(resp.data)
	if data == nil {
		return nil
	}
	return &identity{
		id:       asString(data, "id"),
		username: asString(data, "username"),
		email:    asString(data, "email"),
		name:     asString(data, "name"),
	}
}

type projectInfo struct {
	slug, name, platform string
}

func (c *SentryClient) fetchProjects(ctx context.Context, limit int) []projectInfo {
	resp, err := c.request(ctx, "GET", "/organizations/"+c.OrgSlug+"/projects/", map[string]any{
		"per_page": clampLimit(limit, 100),
	}, nil)
	if err != nil {
		return nil
	}
	var out []projectInfo
	for _, p := range toArray(resp.data) {
		proj := toObject(p)
		out = append(out, projectInfo{
			slug:     asString(proj, "slug"),
			name:     asString(proj, "name"),
			platform: asString(proj, "platform"),
		})
	}
	return out
}

func (c *SentryClient) listProjects(ctx context.Context, limit int, cursor string) (toolResult, error) {
	params := map[string]any{"per_page": clampLimit(limit, 100)}
	if cursor != "" {
		params["cursor"] = cursor
	}
	resp, err := c.request(ctx, "GET", "/organizations/"+c.OrgSlug+"/projects/", params, nil)
	if err != nil {
		return toolResult{}, err
	}
	arr := toArray(resp.data)
	if len(arr) == 0 {
		return textResult("No projects found."), nil
	}
	rows := make([]map[string]any, 0, len(arr))
	for _, p := range arr {
		proj := toObject(p)
		rows = append(rows, map[string]any{
			"id":       proj["id"],
			"slug":     proj["slug"],
			"name":     proj["name"],
			"platform": proj["platform"],
		})
	}
	dropMirror(rows, "name", "slug")
	return jsonResult(ctx, compactTable("projects", rows, map[string]any{
		"org":         c.OrgSlug,
		"next_cursor": c.parseNextCursor(resp.linkHeader),
	})), nil
}

func (c *SentryClient) listTeams(ctx context.Context, limit int, cursor string) (toolResult, error) {
	params := map[string]any{"per_page": clampLimit(limit, 100)}
	if cursor != "" {
		params["cursor"] = cursor
	}
	resp, err := c.request(ctx, "GET", "/organizations/"+c.OrgSlug+"/teams/", params, nil)
	if err != nil {
		return toolResult{}, err
	}
	arr := toArray(resp.data)
	if len(arr) == 0 {
		return textResult("No teams found."), nil
	}
	// Asking for more columns than a given Sentry version populates costs
	// nothing: tabular drops whichever of these come back null for every team.
	rows := make([]map[string]any, 0, len(arr))
	for _, t := range arr {
		team := toObject(t)
		rows = append(rows, map[string]any{
			"id":          team["id"],
			"slug":        team["slug"],
			"name":        team["name"],
			"memberCount": team["memberCount"],
			"isMember":    team["isMember"],
		})
	}
	dropMirror(rows, "name", "slug")
	return jsonResult(ctx, compactTable("teams", rows, map[string]any{
		"next_cursor": c.parseNextCursor(resp.linkHeader),
	})), nil
}

func (c *SentryClient) listUsers(ctx context.Context, query string, limit int, cursor string) (toolResult, error) {
	params := map[string]any{"per_page": clampLimit(limit, 25)}
	if query != "" {
		params["query"] = query
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	resp, err := c.request(ctx, "GET", "/organizations/"+c.OrgSlug+"/members/", params, nil)
	if err != nil {
		return toolResult{}, err
	}
	arr := toArray(resp.data)
	if len(arr) == 0 {
		msg := "No users found"
		if query != "" {
			msg += fmt.Sprintf(" matching %q", query)
		}
		return textResult(msg + "."), nil
	}
	rows := make([]map[string]any, 0, len(arr))
	for _, m := range arr {
		member := toObject(m)
		// The nested user object wins wherever it has a value; the member
		// record fills in for invited accounts that have no user yet.
		merged := map[string]any{}
		for k, v := range member {
			merged[k] = v
		}
		for k, v := range toObject(member["user"]) {
			if v != nil && v != "" {
				merged[k] = v
			}
		}
		rows = append(rows, map[string]any{
			"username": pick(merged, "username", "email"),
			"name":     pick(merged, "name"),
			"email":    pick(merged, "email"),
			"role":     merged["role"],
		})
	}
	dropMirror(rows, "email", "username")
	dropMirror(rows, "name", "username")
	return jsonResult(ctx, compactTable("users", rows, map[string]any{
		"next_cursor": c.parseNextCursor(resp.linkHeader),
	})), nil
}

func (c *SentryClient) getDevContext(ctx context.Context, req *mcp.CallToolRequest) (toolResult, error) {
	// Identity, the two issue queries and the client's roots are independent of
	// each other, so they run together. This is the highest-frequency call in
	// the server and it used to pay four sequential round trips.
	var (
		me               *identity
		roots            []mcpRoot
		assigned, recent []any
		assignedErr      error
		wg               sync.WaitGroup
	)
	issues := func(dst *[]any, errDst *error, params map[string]any) func() {
		return func() {
			defer wg.Done()
			resp, err := c.request(ctx, "GET", "/organizations/"+c.OrgSlug+"/issues/", params, nil)
			if err != nil {
				if errDst != nil {
					*errDst = err
				}
				return
			}
			*dst = toArray(resp.data)
		}
	}
	wg.Add(4)
	go func() { defer wg.Done(); me = c.whoami(ctx) }()
	go func() { defer wg.Done(); roots = resolveRoots(ctx, req) }()
	go issues(&assigned, &assignedErr, map[string]any{"query": "is:unresolved assigned:me", "limit": 10})()
	go issues(&recent, nil, map[string]any{"query": "is:unresolved", "limit": 5, "sort": "new"})()
	wg.Wait()

	var lines []string
	lines = append(lines, "Sentry instance: "+c.baseURL)
	lines = append(lines, "Organization:    "+c.OrgSlug)
	if me != nil {
		id := me.ident()
		if id == "" {
			id = "(unknown)"
		}
		suffix := ""
		if me.email != "" && me.email != me.username {
			suffix = " <" + me.email + ">"
		}
		lines = append(lines, "You:             "+id+suffix)
	} else {
		lines = append(lines, "You:             (could not fetch — check token scopes: org:read)")
	}

	// Workspace roots handed to the server by the MCP client (roots/list or a
	// proxy-set header). These are the repo/working-tree a shell-calling tool
	// would operate in.
	if len(roots) > 0 {
		lines = append(lines, "")
		lines = append(lines, "Workspace roots (from MCP client):")
		for _, r := range roots {
			label := r.path()
			if r.Name != "" {
				label = r.Name + " — " + label
			}
			lines = append(lines, "  • "+label)
		}
	}

	renderIssues := func(issues []any) []string {
		var out []string
		for _, i := range issues {
			issue := toObject(i)
			scope := ""
			if project := toObject(issue["project"]); project != nil {
				if slug := asString(project, "slug"); slug != "" {
					scope = " (" + slug + ")"
				}
			}
			label := issue["shortId"]
			if label == nil {
				label = issue["id"]
			}
			out = append(out, fmt.Sprintf("  • [%v] %v%s", label, issue["title"], scope))
		}
		return out
	}

	lines = append(lines, "")
	switch {
	case assignedErr != nil:
		lines = append(lines, "Could not fetch assigned issues: "+assignedErr.Error())
	case len(assigned) > 0:
		lines = append(lines, fmt.Sprintf("Unresolved issues assigned to you (%d):", len(assigned)))
		lines = append(lines, renderIssues(assigned)...)
	default:
		lines = append(lines, "No unresolved issues assigned to you.")
	}

	if len(recent) > 0 {
		lines = append(lines, "")
		lines = append(lines, "Recent unresolved issues across the org (top 5):")
		lines = append(lines, renderIssues(recent)...)
	}

	// The short IDs above are accepted directly by every issue tool, so these
	// hints name that path rather than sending the reader via a project list.
	lines = append(lines,
		"",
		"Next steps:",
		"  • sentry_search status=unresolved — issues across the whole org (add projectSlug=<slug> to scope)",
		"  • sentry_get_issue issueIdOrUrl=<short-id|id|url> — drill into one of the issues above",
		"  • sentry_stack_frames issueIdOrUrl=<short-id|id|url> — its stack trace, in one call",
	)
	return textResult(strings.Join(lines, "\n")), nil
}

func (c *SentryClient) listIssues(ctx context.Context, projectSlug, query, status string, limit int, cursor string) (toolResult, error) {
	params := map[string]any{}
	q := query
	if status != "" {
		if q != "" {
			q = q + " is:" + status
		} else {
			q = "is:" + status
		}
	}
	if q != "" {
		params["query"] = q
	}
	params["limit"] = clampLimit(limit, 25)
	if cursor != "" {
		params["cursor"] = cursor
	}

	// Without a project this searches the whole organization, which is the
	// same endpoint sentry_get_dev_context uses. Requiring a slug here only
	// forced a list-projects round trip first.
	path := "/organizations/" + c.OrgSlug + "/issues/"
	scope := c.OrgSlug + " (all projects)"
	if projectSlug != "" {
		path = "/projects/" + c.OrgSlug + "/" + projectSlug + "/issues/"
		scope = projectSlug
	}

	resp, err := c.request(ctx, "GET", path, params, nil)
	if err != nil {
		return toolResult{}, err
	}
	arr := toArray(resp.data)
	if len(arr) == 0 {
		if q != "" {
			return textResult(fmt.Sprintf("No issues in %s matching %q.", scope, q)), nil
		}
		return textResult(fmt.Sprintf("No issues in %s.", scope)), nil
	}
	rows := make([]map[string]any, 0, len(arr))
	for _, i := range arr {
		rows = append(rows, issueRow(toObject(i)))
	}
	return jsonResult(ctx, compactTable("issues", rows, map[string]any{
		"scope":       scope,
		"next_cursor": c.parseNextCursor(resp.linkHeader),
	})), nil
}

// ── Issue read ───────────────────────────────────────────────────────────────

func (c *SentryClient) getIssue(ctx context.Context, issueId string, includeLatestEvent bool, includeFields, excludeFields []string, grepPattern string, maxStackFrames *int) (toolResult, error) {
	resp, err := c.request(ctx, "GET", "/issues/"+issueId+"/", nil, nil)
	if err != nil {
		return toolResult{}, err
	}
	out := issueRow(toObject(resp.data))
	out["_full"] = "issues/" + issueId + "/"

	if includeLatestEvent {
		maxFrames := maxEventFrames
		if maxStackFrames != nil {
			maxFrames = *maxStackFrames
		}
		evPath := "organizations/" + c.OrgSlug + "/issues/" + issueId + "/events/latest/"
		evResp, err := c.request(ctx, "GET", "/"+evPath, nil, nil)
		if err != nil {
			out["latest_event"] = map[string]any{"_error": err.Error()}
		} else {
			ev := toObject(evResp.data)
			entries := toArray(ev["entries"])
			projected := make([]any, 0, maxIssueEventEntries)
			for _, e := range entries {
				if len(projected) >= maxIssueEventEntries {
					break
				}
				projected = append(projected, eventEntry(toObject(e), maxFrames))
			}
			latest := map[string]any{
				"eventID":     pick(ev, "eventID", "id"),
				"dateCreated": secs(ev["dateCreated"]),
				"entries":     projected,
				"_full":       evPath,
			}
			if len(entries) > len(projected) {
				latest["entries_omitted"] = len(entries) - len(projected)
			}
			out["latest_event"] = latest
		}
	}

	var filtered any = out
	if len(includeFields) > 0 || len(excludeFields) > 0 {
		filtered = filterFields(filtered, includeFields, excludeFields)
	}
	if grepPattern != "" {
		out, err := grepRendered(ctx, filtered, grepPattern)
		if err != nil {
			return toolResult{}, err
		}
		return textResult(out), nil
	}
	return jsonResult(ctx, filtered), nil
}

func (c *SentryClient) getEvent(ctx context.Context, projectSlug, eventId, issueRef string, limit, offset int, entryType string) (toolResult, error) {
	ref, err := c.resolveEvent(ctx, projectSlug, eventId, issueRef)
	if err != nil {
		return toolResult{}, err
	}
	endpoint := ref.endpoint
	ev, err := c.fetchEvent(ctx, ref)
	if err != nil {
		return toolResult{}, err
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 5
	}

	out := map[string]any{
		"eventID": pick(ev, "eventID", "id"), "dateCreated": secs(ev["dateCreated"]),
		"message": ev["message"], "title": ev["title"], "platform": ev["platform"],
		"_full": endpoint,
	}

	if entries := toArray(ev["entries"]); entries != nil {
		total := len(entries)
		var selected []any

		if entryType != "" {
			var filtered []any
			for _, e := range entries {
				if asString(toObject(e), "type") == entryType {
					filtered = append(filtered, e)
				}
			}
			selected = sliceRange(filtered, offset, limit)
		} else {
			priority := []string{"exception", "message", "breadcrumbs", "request"}
			var top []any
			for _, t := range priority {
				if len(top) >= limit {
					break
				}
				for _, e := range entries {
					if asString(toObject(e), "type") == t {
						top = append(top, e)
						break
					}
				}
			}
			if len(top) < limit {
				for _, e := range entries {
					if len(top) >= limit {
						break
					}
					if !contains(priority, asString(toObject(e), "type")) {
						top = append(top, e)
					}
				}
			}
			selected = top
		}

		outEntries := make([]any, 0, len(selected))
		for _, e := range selected {
			outEntries = append(outEntries, eventEntry(toObject(e), maxEventFrames))
		}
		out["entries"] = outEntries

		availableTypes := []any{}
		seen := map[string]bool{}
		for _, e := range entries {
			t := asString(toObject(e), "type")
			if !seen[t] {
				seen[t] = true
				availableTypes = append(availableTypes, t)
			}
		}
		// Counts and the type list only. How to widen the selection is the
		// same sentence on every call, so it is stated once in the server
		// instructions instead of being re-sent with each response.
		out["entries_info"] = map[string]any{
			"total":           total,
			"showing":         len(selected),
			"available_types": availableTypes,
		}
	}
	return jsonResult(ctx, out), nil
}

func sliceRange(arr []any, offset, limit int) []any {
	if offset >= len(arr) {
		return []any{}
	}
	end := offset + limit
	if end > len(arr) {
		end = len(arr)
	}
	return arr[offset:end]
}

func contains(arr []string, s string) bool {
	for _, x := range arr {
		if x == s {
			return true
		}
	}
	return false
}

// ── Issue mutation ───────────────────────────────────────────────────────────

func (c *SentryClient) updateIssueStatus(ctx context.Context, issueId, status string) (string, error) {
	resp, err := c.request(ctx, "PUT", "/issues/"+issueId+"/", nil, map[string]any{"status": status})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Issue %s → %v", issueId, toObject(resp.data)["status"]), nil
}

// assignIssue assigns the issue. assignedTo == "" unassigns.
// assignIssue assigns the issue. assignedTo == "" unassigns.
//
// Sentry accepts an actor string it cannot resolve and simply leaves the issue
// unassigned, so the response is checked rather than trusted: a request that
// did not stick is reported as an error instead of a cheerful confirmation.
func (c *SentryClient) assignIssue(ctx context.Context, issueId, assignedTo string) (string, error) {
	resp, err := c.request(ctx, "PUT", "/issues/"+issueId+"/", nil, map[string]any{"assignedTo": assignedTo})
	if err != nil {
		return "", err
	}
	assignee := toObject(toObject(resp.data)["assignedTo"])
	label := ""
	for _, k := range []string{"username", "name", "email", "slug"} {
		if s := asString(assignee, k); s != "" {
			label = s
			break
		}
	}
	if assignedTo == "" {
		if label != "" {
			return "", fmt.Errorf("Issue %s is still assigned to %s after an unassign request.", issueId, label)
		}
		return fmt.Sprintf("Issue %s assignee → (unassigned)", issueId), nil
	}
	if label == "" {
		return "", fmt.Errorf("Sentry did not accept %q as an assignee — issue %s is still unassigned. Look the username up with sentry_search resource=users.", assignedTo, issueId)
	}
	return fmt.Sprintf("Issue %s assignee → %s", issueId, label), nil
}

// mutateIssue applies status/assignee/comment in one call. statusSet/assignSet
// indicate whether the respective field was provided.
func (c *SentryClient) mutateIssue(ctx context.Context, issueId, status string, statusSet bool, assignedTo string, assignSet bool, comment string) (toolResult, error) {
	var lines []string
	if statusSet {
		r, err := c.updateIssueStatus(ctx, issueId, status)
		if err != nil {
			return toolResult{}, err
		}
		lines = append(lines, r)
	}
	if assignSet {
		// An email or a real name is resolved to a username here, so the
		// caller does not have to make a lookup call first to avoid a silent
		// no-op. An ambiguous match is an error listing the candidates.
		actor, err := c.resolveAssignee(ctx, assignedTo)
		if err != nil {
			return toolResult{}, err
		}
		r, err := c.assignIssue(ctx, issueId, actor)
		if err != nil {
			return toolResult{}, err
		}
		lines = append(lines, r)
	}
	if strings.TrimSpace(comment) != "" {
		r, err := c.addComment(ctx, issueId, comment)
		if err != nil {
			return toolResult{}, err
		}
		lines = append(lines, r)
	}
	if len(lines) == 0 {
		return textResult("No mutations specified. Provide status, assignedTo, or comment."), nil
	}
	return textResult(strings.Join(lines, "\n")), nil
}

// ── Comments ─────────────────────────────────────────────────────────────────

func (c *SentryClient) addComment(ctx context.Context, issueId, body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("Comment body must not be empty.")
	}
	resp, err := c.request(ctx, "POST", "/issues/"+issueId+"/comments/", nil, map[string]any{"text": trimmed})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Added comment %v on issue %s.", toObject(resp.data)["id"], issueId), nil
}

func (c *SentryClient) editComment(ctx context.Context, issueId, commentId, body string) (toolResult, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return toolResult{}, fmt.Errorf("Comment body must not be empty.")
	}
	resp, err := c.request(ctx, "PUT", "/issues/"+issueId+"/comments/"+commentId+"/", nil, map[string]any{"text": trimmed})
	if err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Updated comment %v on issue %s.", toObject(resp.data)["id"], issueId)), nil
}

func (c *SentryClient) deleteComment(ctx context.Context, issueId, commentId string) (toolResult, error) {
	if _, err := c.request(ctx, "DELETE", "/issues/"+issueId+"/comments/"+commentId+"/", nil, nil); err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Deleted comment %s on issue %s.", commentId, issueId)), nil
}

// ── Specialized debug tools ──────────────────────────────────────────────────

func (c *SentryClient) getStackFrames(ctx context.Context, projectSlug, eventId, issueRef string, inAppOnly bool, maxFrames int) (toolResult, error) {
	ref, err := c.resolveEvent(ctx, projectSlug, eventId, issueRef)
	if err != nil {
		return toolResult{}, err
	}
	endpoint := ref.endpoint
	ev, err := c.fetchEvent(ctx, ref)
	if err != nil {
		return toolResult{}, err
	}
	var raw []map[string]any
	for _, e := range toArray(ev["entries"]) {
		entry := toObject(e)
		if asString(entry, "type") != "exception" {
			continue
		}
		for _, v := range toArray(toObject(entry["data"])["values"]) {
			for _, f := range toArray(toObject(toObject(v)["stacktrace"])["frames"]) {
				frame := toObject(f)
				if frame == nil {
					continue
				}
				inApp, _ := frame["in_app"].(bool)
				if inAppOnly && !inApp {
					continue
				}
				raw = append(raw, frame)
			}
		}
	}
	if maxFrames <= 0 {
		maxFrames = 50
	}
	total := len(raw)
	kept := raw
	if total > maxFrames {
		kept = raw[total-maxFrames:]
	}

	rows := make([]map[string]any, 0, len(kept))
	for _, f := range kept {
		rows = append(rows, frameRow(f))
	}

	extra := map[string]any{
		"eventID":     pick(ev, "eventID", "id"),
		"totalFrames": total,
		"_full":       endpoint,
	}
	if total > len(kept) {
		extra["frames_omitted"] = total - len(kept)
	}
	// Source context for the one frame a reader starts from, rather than seven
	// lines of it on every frame — which on a long trace is most of the reply.
	if src := frameSource(deepestInApp(kept)); src != nil {
		extra["source"] = src
	}
	return jsonResult(ctx, compactTable("frames", rows, extra)), nil
}

func (c *SentryClient) checkDsymStatus(ctx context.Context, projectSlug, eventId, issueRef string) (toolResult, error) {
	ref, err := c.resolveEvent(ctx, projectSlug, eventId, issueRef)
	if err != nil {
		return toolResult{}, err
	}
	ev, err := c.fetchEvent(ctx, ref)
	if err != nil {
		return toolResult{}, err
	}

	rows := []map[string]any{}
	for _, e := range toArray(ev["errors"]) {
		errObj := toObject(e)
		t := asString(errObj, "type")
		if t != "native_missing_dsym" && t != "proguard_missing_mapping" {
			continue
		}
		errData := toObject(errObj["data"])
		rows = append(rows, map[string]any{
			"type":      errObj["type"],
			"imageUuid": errData["image_uuid"],
			"imageName": errData["image_name"],
			"imagePath": errData["image_path"],
		})
	}
	// count carries whether anything is missing, so no separate boolean; the
	// command to run is kept because it is the action, not advice about it.
	extra := map[string]any{
		"project": firstNonEmpty(ref.project, asString(toObject(ev["project"]), "slug")),
		"eventID": pick(ev, "eventID", "id"),
		"_full":   ref.endpoint,
	}
	if len(rows) > 0 {
		extra["upload"] = "sentry-cli upload-dif <path>"
	}
	return jsonResult(ctx, compactTable("missingSymbols", rows, extra)), nil
}

func (c *SentryClient) rawApi(ctx context.Context, endpoint, method string, params map[string]any, body any, path, grepPattern string, maxChars, charOffset int, wantOutline bool) (toolResult, error) {
	method = strings.ToUpper(method)
	if method == "" {
		method = "GET"
	}
	if !contains([]string{"GET", "POST", "PUT", "DELETE"}, method) {
		return toolResult{}, fmt.Errorf("Unsupported HTTP method: %s", method)
	}
	// Strip optional /api/0/ prefix so callers can copy URLs from docs.
	endpoint = regexp.MustCompile(`^/?api/0/`).ReplaceAllString(endpoint, "/")
	endpoint = regexp.MustCompile(`^/+`).ReplaceAllString(endpoint, "/")
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}

	resp, err := c.request(ctx, method, endpoint, params, body)
	if err != nil {
		return toolResult{}, err
	}

	// path walks into the payload before anything is rendered, which is how a
	// `_full` endpoint from a compact response is drilled: one subtree crosses
	// the wire instead of the whole document.
	data := resp.data
	if path != "" {
		data, err = walkPath(data, path)
		if err != nil {
			return toolResult{}, fmt.Errorf("path %q: %v", path, err)
		}
	}
	if wantOutline {
		return jsonResult(ctx, map[string]any{
			"endpoint": endpoint,
			"path":     path,
			"shape":    outline(data, 0),
		}), nil
	}
	if grepPattern != "" {
		out, err := grepRendered(ctx, data, grepPattern)
		if err != nil {
			return toolResult{}, err
		}
		return pageText(out, maxChars, charOffset), nil
	}
	rendered := renderString(ctx, data)

	// Explicit paging takes precedence over the size guard below.
	if charOffset > 0 || maxChars > 0 {
		return pageText(rendered, maxChars, charOffset), nil
	}

	// Too large to hand over whole. Returning the shape instead of a wall of
	// grep suggestions makes the next call a path into the part that matters.
	if grepPattern == "" && len(rendered) > rawApiOutlineAt {
		return jsonResult(ctx, map[string]any{
			"endpoint": endpoint,
			"shape":    outline(data, 0),
			"_note": fmt.Sprintf("Response is ~%d tokens, so this is its shape, not its contents. Re-run with path=<dotted.path> for one subtree, grepPattern=<regex> to filter, or maxChars to page.",
				(len(rendered)+3)/4),
		}), nil
	}
	return textResult(rendered), nil
}

// rawApiOutlineAt is the rendered size (~20k tokens) past which sentry_raw_api
// returns a shape sketch instead of the payload.
const rawApiOutlineAt = 80000
