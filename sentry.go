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
	"strings"
	"time"

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

// renderFormat selects how structured data tools serialize their output:
// "toon" (default — token-efficient) or "json". Set per request in callTool.
// The MCP request loop is serial, so a package-level var is safe.
var renderFormat = "toon"

// renderString serializes v in the active output format. TOON is the default;
// on any encoding error it falls back to pretty JSON.
func renderString(v any) string {
	if renderFormat == "json" {
		return marshalIndent(v)
	}
	s, err := toon.MarshalString(v)
	if err != nil {
		return marshalIndent(v)
	}
	return s
}

// jsonResult renders structured data as a text tool result.
func jsonResult(v any) toolResult {
	return textResult(renderString(v))
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

// grepFilter filters pretty-printed JSON to lines matching pattern (plus one
// line of context on each side) and attempts to re-parse the result as JSON.
func grepFilter(data any, pattern string) (any, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid grepPattern: %v", err)
	}
	jsonStr := marshalIndent(data)
	lines := strings.Split(jsonStr, "\n")
	var matched []string
	for i, line := range lines {
		if re.MatchString(line) {
			if i > 0 {
				matched = append(matched, lines[i-1])
			}
			matched = append(matched, line)
			if i < len(lines)-1 {
				matched = append(matched, lines[i+1])
			}
		}
	}
	filtered := strings.Join(matched, "\n")
	var parsed any
	if err := json.Unmarshal([]byte(filtered), &parsed); err == nil {
		return parsed, nil
	}
	return map[string]any{"grep_results": matched, "original_pattern": pattern}, nil
}

// truncateStackFrames trims exception stack traces inside event entries to the
// last maxFrames frames, recording how many were omitted.
func truncateStackFrames(data any, maxFrames int) any {
	switch v := data.(type) {
	case []any:
		for i, item := range v {
			v[i] = truncateStackFrames(item, maxFrames)
		}
		return v
	case map[string]any:
		entries, _ := v["entries"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if entry == nil || asString(entry, "type") != "exception" {
				continue
			}
			edata, _ := entry["data"].(map[string]any)
			values, _ := edata["values"].([]any)
			for _, val := range values {
				value, _ := val.(map[string]any)
				stacktrace, _ := value["stacktrace"].(map[string]any)
				frames, _ := stacktrace["frames"].([]any)
				if len(frames) > maxFrames {
					stacktrace["frames"] = frames[len(frames)-maxFrames:]
					stacktrace["frames_omitted"] = len(frames) - maxFrames
				}
			}
		}
		return v
	default:
		return data
	}
}

func asString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// essentialIssueFields keeps only the metadata needed to keep issue responses compact.
func essentialIssueFields(issue map[string]any) map[string]any {
	keys := []string{
		"id", "shortId", "title", "culprit", "permalink", "logger", "level",
		"status", "type", "platform", "project", "count", "userCount",
		"firstSeen", "lastSeen", "assignedTo", "metadata",
	}
	out := map[string]any{}
	for _, k := range keys {
		out[k] = issue[k]
	}
	return out
}

func essentialEventEntry(entry map[string]any) map[string]any {
	switch asString(entry, "type") {
	case "exception":
		data, _ := entry["data"].(map[string]any)
		values, _ := data["values"].([]any)
		outValues := make([]any, 0, len(values))
		for _, v := range values {
			exc, _ := v.(map[string]any)
			if exc == nil {
				continue
			}
			out := map[string]any{
				"type":      exc["type"],
				"value":     exc["value"],
				"mechanism": exc["mechanism"],
			}
			if stacktrace, ok := exc["stacktrace"].(map[string]any); ok {
				frames, _ := stacktrace["frames"].([]any)
				if len(frames) > 5 {
					frames = frames[len(frames)-5:]
				}
				outFrames := make([]any, 0, len(frames))
				for _, f := range frames {
					frame, _ := f.(map[string]any)
					if frame == nil {
						continue
					}
					fo := map[string]any{
						"filename": frame["filename"],
						"function": frame["function"],
						"lineNo":   frame["lineNo"],
						"colNo":    frame["colNo"],
						"absPath":  frame["absPath"],
						"inApp":    frame["in_app"],
					}
					if ctx, ok := frame["context"].([]any); ok {
						if len(ctx) > 7 {
							ctx = ctx[:7]
						}
						fo["context"] = ctx
					}
					outFrames = append(outFrames, fo)
				}
				out["stacktrace"] = map[string]any{"frames": outFrames}
			} else {
				out["stacktrace"] = nil
			}
			outValues = append(outValues, out)
		}
		return map[string]any{"type": "exception", "data": map[string]any{"values": outValues}}
	case "message":
		return entry
	case "breadcrumbs":
		data, _ := entry["data"].(map[string]any)
		values, _ := data["values"].([]any)
		if len(values) > 10 {
			values = values[len(values)-10:]
		}
		return map[string]any{"type": "breadcrumbs", "data": map[string]any{"values": values}}
	default:
		return map[string]any{"type": entry["type"], "_truncated": true}
	}
}

// ── Sentry client ────────────────────────────────────────────────────────────

type SentryClient struct {
	baseURL string
	OrgSlug string
	token   string
	http    *http.Client
}

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

func (c *SentryClient) request(method, path string, params map[string]any, body any) (apiResponse, error) {
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

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, err
		}
		reqBody = bytes.NewReader(b)
	}

	ctx := callCtx
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return apiResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		errText, _ := io.ReadAll(res.Body)
		return apiResponse{}, fmt.Errorf("%s", formatSentryError(res.StatusCode, method, cleanPath, parseSentryErrorDetails(string(errText))))
	}

	resp := apiResponse{linkHeader: res.Header.Get("link"), status: res.StatusCode}
	if res.StatusCode == 204 {
		return resp, nil
	}
	dec := json.NewDecoder(res.Body)
	if err := dec.Decode(&resp.data); err != nil && err != io.EOF {
		return apiResponse{}, err
	}
	return resp, nil
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

func (c *SentryClient) whoami() *identity {
	// /auth/ works for both user PATs and org auth tokens; /users/me/ rejects org tokens.
	resp, err := c.request("GET", "/auth/", nil, nil)
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

func (c *SentryClient) fetchProjects(limit int) []projectInfo {
	resp, err := c.request("GET", "/organizations/"+c.OrgSlug+"/projects/", map[string]any{
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

func (c *SentryClient) listProjects(limit int, cursor string) (toolResult, error) {
	params := map[string]any{"per_page": clampLimit(limit, 100)}
	if cursor != "" {
		params["cursor"] = cursor
	}
	resp, err := c.request("GET", "/organizations/"+c.OrgSlug+"/projects/", params, nil)
	if err != nil {
		return toolResult{}, err
	}
	arr := toArray(resp.data)
	if len(arr) == 0 {
		return textResult("No projects found."), nil
	}
	lines := []string{fmt.Sprintf("Projects in %q:", c.OrgSlug)}
	for _, p := range arr {
		proj := toObject(p)
		line := fmt.Sprintf("  • %v — %v", proj["slug"], proj["name"])
		if pl := asString(proj, "platform"); pl != "" {
			line += fmt.Sprintf(" (%s)", pl)
		}
		lines = append(lines, line)
	}
	if next := c.parseNextCursor(resp.linkHeader); next != "" {
		lines = append(lines, "", "next_cursor: "+next)
	}
	return textResult(strings.Join(lines, "\n")), nil
}

func (c *SentryClient) listTeams(limit int, cursor string) (toolResult, error) {
	params := map[string]any{"per_page": clampLimit(limit, 100)}
	if cursor != "" {
		params["cursor"] = cursor
	}
	resp, err := c.request("GET", "/organizations/"+c.OrgSlug+"/teams/", params, nil)
	if err != nil {
		return toolResult{}, err
	}
	arr := toArray(resp.data)
	if len(arr) == 0 {
		return textResult("No teams found."), nil
	}
	next := c.parseNextCursor(resp.linkHeader)
	if next == "" {
		return jsonResult(arr), nil
	}
	return jsonResult(map[string]any{"teams": arr, "next_cursor": next}), nil
}

func (c *SentryClient) listUsers(query string, limit int, cursor string) (toolResult, error) {
	params := map[string]any{"per_page": clampLimit(limit, 25)}
	if query != "" {
		params["query"] = query
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	resp, err := c.request("GET", "/organizations/"+c.OrgSlug+"/members/", params, nil)
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
	summary := make([]any, 0, len(arr))
	for _, m := range arr {
		member := toObject(m)
		user := toObject(member["user"])
		coalesce := func(a, fallbackKey string) any {
			if user[a] != nil {
				return user[a]
			}
			return member[fallbackKey]
		}
		summary = append(summary, map[string]any{
			"username": coalesce("username", "email"),
			"name":     coalesce("name", "name"),
			"email":    coalesce("email", "email"),
			"role":     member["role"],
		})
	}
	payload := map[string]any{"users": summary, "count": len(summary)}
	if next := c.parseNextCursor(resp.linkHeader); next != "" {
		payload["next_cursor"] = next
	}
	return jsonResult(payload), nil
}

func (c *SentryClient) getDevContext() (toolResult, error) {
	me := c.whoami()
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
	if roots := activePeer.listRoots(); len(roots) > 0 {
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

	if resp, err := c.request("GET", "/organizations/"+c.OrgSlug+"/issues/", map[string]any{
		"query": "is:unresolved assigned:me", "limit": 10,
	}, nil); err == nil {
		assigned := toArray(resp.data)
		lines = append(lines, "")
		if len(assigned) > 0 {
			lines = append(lines, fmt.Sprintf("Unresolved issues assigned to you (%d):", len(assigned)))
			lines = append(lines, renderIssues(assigned)...)
		} else {
			lines = append(lines, "No unresolved issues assigned to you.")
		}
	} else {
		lines = append(lines, "")
		lines = append(lines, "Could not fetch assigned issues: "+err.Error())
	}

	if resp, err := c.request("GET", "/organizations/"+c.OrgSlug+"/issues/", map[string]any{
		"query": "is:unresolved", "limit": 5, "sort": "new",
	}, nil); err == nil {
		recent := toArray(resp.data)
		if len(recent) > 0 {
			lines = append(lines, "")
			lines = append(lines, "Recent unresolved issues across the org (top 5):")
			lines = append(lines, renderIssues(recent)...)
		}
	}

	lines = append(lines,
		"",
		"Next steps:",
		"  • sentry_search resource=projects — list available projects",
		"  • sentry_search projectSlug=<slug> status=unresolved — list issues for a project",
		"  • sentry_get_issue issueIdOrUrl=<id|url> — drill into a specific issue",
	)
	return textResult(strings.Join(lines, "\n")), nil
}

func (c *SentryClient) listIssues(projectSlug, query, status string, limit int, cursor string) (toolResult, error) {
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
	resp, err := c.request("GET", "/projects/"+c.OrgSlug+"/"+projectSlug+"/issues/", params, nil)
	if err != nil {
		return toolResult{}, err
	}
	arr := toArray(resp.data)
	issues := make([]any, 0, len(arr))
	for _, i := range arr {
		issues = append(issues, essentialIssueFields(toObject(i)))
	}
	payload := map[string]any{"issues": issues, "count": len(issues)}
	if next := c.parseNextCursor(resp.linkHeader); next != "" {
		payload["next_cursor"] = next
	}
	return jsonResult(payload), nil
}

// ── Issue read ───────────────────────────────────────────────────────────────

func (c *SentryClient) getIssue(issueIdOrUrl string, includeLatestEvent bool, includeFields, excludeFields []string, grepPattern string, maxStackFrames *int) (toolResult, error) {
	issueId := extractIssueId(issueIdOrUrl)
	if issueId == "" {
		return toolResult{}, fmt.Errorf("Could not extract issue ID from %q. Pass a numeric ID or full issue URL.", issueIdOrUrl)
	}

	resp, err := c.request("GET", "/issues/"+issueId+"/", nil, nil)
	if err != nil {
		return toolResult{}, err
	}
	combined := essentialIssueFields(toObject(resp.data))
	combined["latest_event"] = nil

	if includeLatestEvent {
		evResp, err := c.request("GET", "/organizations/"+c.OrgSlug+"/issues/"+issueId+"/events/latest/", nil, nil)
		if err != nil {
			combined["latest_event"] = map[string]any{"_error": err.Error()}
		} else {
			ev := toObject(evResp.data)
			rawEntries := toArray(ev["entries"])
			if len(rawEntries) > 3 {
				rawEntries = rawEntries[:3]
			}
			entries := make([]any, 0, len(rawEntries))
			for _, e := range rawEntries {
				entries = append(entries, essentialEventEntry(toObject(e)))
			}
			combined["latest_event"] = map[string]any{
				"id":          ev["id"],
				"eventID":     ev["eventID"],
				"dateCreated": ev["dateCreated"],
				"entries":     entries,
				"_note":       "Event truncated. Use sentry_get_event for full data.",
			}
		}
	}

	var out any = combined
	if maxStackFrames != nil {
		out = truncateStackFrames(out, *maxStackFrames)
	}
	if len(includeFields) > 0 || len(excludeFields) > 0 {
		out = filterFields(out, includeFields, excludeFields)
	}
	if grepPattern != "" {
		filtered, err := grepFilter(out, grepPattern)
		if err != nil {
			return toolResult{}, err
		}
		out = filtered
	}
	return jsonResult(out), nil
}

func (c *SentryClient) getEvent(projectSlug, eventId string, limit, offset int, entryType string) (toolResult, error) {
	resp, err := c.request("GET", "/projects/"+c.OrgSlug+"/"+projectSlug+"/events/"+eventId+"/", nil, nil)
	if err != nil {
		return toolResult{}, err
	}
	ev := toObject(resp.data)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 5
	}

	out := map[string]any{
		"id": ev["id"], "eventID": ev["eventID"], "dateCreated": ev["dateCreated"],
		"message": ev["message"], "title": ev["title"], "platform": ev["platform"],
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
			outEntries = append(outEntries, essentialEventEntry(toObject(e)))
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
		tip := "Showing prioritized entries. Use entryType=\"exception\" to see only stack traces."
		if entryType != "" {
			tip = fmt.Sprintf("Showing only %q entries. Remove entryType to see prioritized entries.", entryType)
		}
		out["pagination_info"] = map[string]any{
			"total_entries":   total,
			"showing":         len(selected),
			"available_types": availableTypes,
			"tip":             tip,
		}
	}
	return jsonResult(out), nil
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

func (c *SentryClient) updateIssueStatus(issueId, status string) (string, error) {
	resp, err := c.request("PUT", "/issues/"+issueId+"/", nil, map[string]any{"status": status})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Issue %s → %v", issueId, toObject(resp.data)["status"]), nil
}

// assignIssue assigns the issue. assignedTo == "" unassigns.
func (c *SentryClient) assignIssue(issueId, assignedTo string) (string, error) {
	resp, err := c.request("PUT", "/issues/"+issueId+"/", nil, map[string]any{"assignedTo": assignedTo})
	if err != nil {
		return "", err
	}
	label := "(unassigned)"
	if a := toObject(toObject(resp.data)["assignedTo"]); a != nil {
		for _, k := range []string{"username", "name", "email"} {
			if s := asString(a, k); s != "" {
				label = s
				break
			}
		}
	}
	return fmt.Sprintf("Issue %s assignee → %s", issueId, label), nil
}

// mutateIssue applies status/assignee/comment in one call. statusSet/assignSet
// indicate whether the respective field was provided.
func (c *SentryClient) mutateIssue(issueId, status string, statusSet bool, assignedTo string, assignSet bool, comment string) (toolResult, error) {
	var lines []string
	if statusSet {
		r, err := c.updateIssueStatus(issueId, status)
		if err != nil {
			return toolResult{}, err
		}
		lines = append(lines, r)
	}
	if assignSet {
		r, err := c.assignIssue(issueId, assignedTo)
		if err != nil {
			return toolResult{}, err
		}
		lines = append(lines, r)
	}
	if strings.TrimSpace(comment) != "" {
		r, err := c.addComment(issueId, comment)
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

func (c *SentryClient) addComment(issueId, body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("Comment body must not be empty.")
	}
	resp, err := c.request("POST", "/issues/"+issueId+"/comments/", nil, map[string]any{"text": trimmed})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Added comment %v on issue %s.", toObject(resp.data)["id"], issueId), nil
}

func (c *SentryClient) editComment(issueId, commentId, body string) (toolResult, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return toolResult{}, fmt.Errorf("Comment body must not be empty.")
	}
	resp, err := c.request("PUT", "/issues/"+issueId+"/comments/"+commentId+"/", nil, map[string]any{"text": trimmed})
	if err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Updated comment %v on issue %s.", toObject(resp.data)["id"], issueId)), nil
}

func (c *SentryClient) deleteComment(issueId, commentId string) (toolResult, error) {
	if _, err := c.request("DELETE", "/issues/"+issueId+"/comments/"+commentId+"/", nil, nil); err != nil {
		return toolResult{}, err
	}
	return textResult(fmt.Sprintf("Deleted comment %s on issue %s.", commentId, issueId)), nil
}

// ── Specialized debug tools ──────────────────────────────────────────────────

func (c *SentryClient) getStackFrames(projectSlug, eventId string, inAppOnly bool, maxFrames int) (toolResult, error) {
	resp, err := c.request("GET", "/projects/"+c.OrgSlug+"/"+projectSlug+"/events/"+eventId+"/", nil, nil)
	if err != nil {
		return toolResult{}, err
	}
	ev := toObject(resp.data)
	var frames []any
	for _, e := range toArray(ev["entries"]) {
		entry := toObject(e)
		if asString(entry, "type") != "exception" {
			continue
		}
		data := toObject(entry["data"])
		for _, v := range toArray(data["values"]) {
			exc := toObject(v)
			stacktrace := toObject(exc["stacktrace"])
			for _, f := range toArray(stacktrace["frames"]) {
				frame := toObject(f)
				inApp, _ := frame["in_app"].(bool)
				if inAppOnly && !inApp {
					continue
				}
				fn := frame["function"]
				if fn == nil {
					fn = frame["rawFunction"]
				}
				if fn == nil {
					fn = "<unknown>"
				}
				filename := frame["filename"]
				if filename == nil {
					filename = frame["absPath"]
				}
				frames = append(frames, map[string]any{
					"function":        fn,
					"filename":        filename,
					"lineNo":          frame["lineNo"],
					"colNo":           frame["colNo"],
					"inApp":           inApp,
					"module":          frame["module"],
					"package":         frame["package"],
					"instructionAddr": frame["instructionAddr"],
					"symbolAddr":      frame["symbolAddr"],
				})
			}
		}
	}
	if maxFrames <= 0 {
		maxFrames = 50
	}
	limited := frames
	if len(frames) > maxFrames {
		limited = frames[len(frames)-maxFrames:]
	}
	if limited == nil {
		limited = []any{}
	}
	return jsonResult(map[string]any{
		"eventId":        eventId,
		"totalFrames":    len(frames),
		"returnedFrames": len(limited),
		"inAppOnly":      inAppOnly,
		"frames":         limited,
	}), nil
}

func (c *SentryClient) checkDsymStatus(projectSlug, eventId string) (toolResult, error) {
	var ev map[string]any
	if eventId != "" {
		resp, err := c.request("GET", "/projects/"+c.OrgSlug+"/"+projectSlug+"/events/"+eventId+"/", nil, nil)
		if err != nil {
			return toolResult{}, err
		}
		ev = toObject(resp.data)
	} else {
		resp, err := c.request("GET", "/projects/"+c.OrgSlug+"/"+projectSlug+"/issues/", map[string]any{"limit": 1}, nil)
		if err != nil {
			return toolResult{}, err
		}
		issues := toArray(resp.data)
		if len(issues) == 0 {
			return textResult("No recent issues found in project. Cannot check dSYM status."), nil
		}
		issueId := toObject(issues[0])["id"]
		evResp, err := c.request("GET", fmt.Sprintf("/organizations/%s/issues/%v/events/latest/", c.OrgSlug, issueId), nil, nil)
		if err != nil {
			return toolResult{}, err
		}
		ev = toObject(evResp.data)
	}

	var missing []any
	for _, e := range toArray(ev["errors"]) {
		errObj := toObject(e)
		t := asString(errObj, "type")
		if t == "native_missing_dsym" || t == "proguard_missing_mapping" {
			errData := toObject(errObj["data"])
			missing = append(missing, map[string]any{
				"type":      errObj["type"],
				"message":   errObj["message"],
				"imagePath": errData["image_path"],
				"imageUuid": errData["image_uuid"],
				"imageName": errData["image_name"],
			})
		}
	}
	if missing == nil {
		missing = []any{}
	}
	recommendation := "All debug symbols are present for this event."
	if len(missing) > 0 {
		recommendation = "Upload missing dSYM files to Sentry — sentry-cli upload-dif <path> — to see function names instead of addresses."
	}
	eid := ev["eventID"]
	if eid == nil {
		eid = eventId
	}
	return jsonResult(map[string]any{
		"project":           projectSlug,
		"eventId":           eid,
		"hasMissingSymbols": len(missing) > 0,
		"missingCount":      len(missing),
		"missingSymbols":    missing,
		"recommendation":    recommendation,
	}), nil
}

func (c *SentryClient) rawApi(endpoint, method string, params map[string]any, body any, grepPattern string, maxChars, charOffset int) (toolResult, error) {
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

	resp, err := c.request(method, endpoint, params, body)
	if err != nil {
		return toolResult{}, err
	}

	var filtered any = resp.data
	if grepPattern != "" {
		filtered, err = grepFilter(resp.data, grepPattern)
		if err != nil {
			return toolResult{}, err
		}
	}
	jsonStr := renderString(filtered)

	// Explicit paging takes precedence over the token-size warning. Slice on
	// runes, not bytes, so multibyte UTF-8 (common in Sentry payloads) is never
	// split mid-character.
	if charOffset > 0 || maxChars > 0 {
		runes := []rune(jsonStr)
		offset := charOffset
		if offset > len(runes) {
			offset = len(runes)
		}
		limit := maxChars
		if limit <= 0 {
			limit = len(runes)
		}
		end := offset + limit
		if end > len(runes) {
			end = len(runes)
		}
		chunk := string(runes[offset:end])
		remaining := len(runes) - end
		suffix := ""
		if remaining > 0 {
			suffix = fmt.Sprintf("\n\n... (%d more chars, use charOffset=%d)", remaining, end)
		}
		return textResult(chunk + suffix), nil
	}

	estimatedTokens := (len(jsonStr) + 3) / 4
	if estimatedTokens > 20000 && grepPattern == "" {
		return textResult(strings.Join([]string{
			fmt.Sprintf("WARNING: Response is approximately %d tokens (%d chars).", estimatedTokens, len(jsonStr)),
			"",
			"This endpoint returns a lot of data. Re-run with one of:",
			"  - grepPattern=\"...\" to filter inline",
			"  - maxChars=8000 charOffset=0 to page through",
			"",
			"Suggested grep patterns:",
			"  - Stack frames: '\"function\":|\"filename\":|\"in_app\":'",
			"  - Breadcrumbs:  '\"breadcrumbs\"'",
			"  - Tags:         '\"tags\"'",
		}, "\n")), nil
	}
	return textResult(jsonStr), nil
}
