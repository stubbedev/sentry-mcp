package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stub serves canned JSON per API path (the part after /api/0) and records
// every path requested, so a test can assert which endpoint a resolver chose.
type stub struct {
	mu     sync.Mutex
	routes map[string]string
	seen   []string
	srv    *httptest.Server
}

func newStub(t *testing.T, routes map[string]string) (*SentryClient, *stub) {
	t.Helper()
	s := &stub{routes: routes}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/0")
		s.mu.Lock()
		s.seen = append(s.seen, path)
		body, ok := s.routes[path]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"detail":"not found"}`))
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return NewSentryClient(s.srv.URL, "tok", "konform"), s
}

func (s *stub) requested(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.seen {
		if p == path {
			return true
		}
	}
	return false
}

func (s *stub) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func TestShortIdFromInput(t *testing.T) {
	cases := map[string]string{
		"KONTAINER-BACKEND-4JH": "KONTAINER-BACKEND-4JH",
		"kontainer-backend-4jh": "KONTAINER-BACKEND-4JH", // callers paste either case
		"https://sentry.konform.com/organizations/konform/issues/KONTAINER-BACKEND-4JH/": "KONTAINER-BACKEND-4JH",
		"https://sentry.konform.com/organizations/konform/issues/?query=PROJECT-ABC":     "PROJECT-ABC",
		"not a short id": "",
		"":               "",
	}
	for in, want := range cases {
		if got := shortIdFromInput(in); got != want {
			t.Errorf("shortIdFromInput(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveIssueId(t *testing.T) {
	c, s := newStub(t, map[string]string{
		"/organizations/konform/shortids/KONTAINER-BACKEND-4JH/": `{"shortId":"KONTAINER-BACKEND-4JH","groupId":"60741"}`,
		"/organizations/konform/shortids/GROUP-ONLY/":            `{"shortId":"GROUP-ONLY","group":{"id":"777"}}`,
	})
	ctx := context.Background()

	// A numeric id and a URL resolve locally, with no request at all.
	for _, in := range []string{"60741", "https://sentry.konform.com/organizations/konform/issues/60741/"} {
		got, err := c.resolveIssueId(ctx, in)
		if err != nil || got != "60741" {
			t.Errorf("resolveIssueId(%q) = %q, %v; want 60741", in, got, err)
		}
	}
	if len(s.paths()) != 0 {
		t.Errorf("a numeric id should need no lookup, but hit %v", s.paths())
	}

	// A short id — the identifier sentry_get_dev_context prints — resolves via
	// the shortids endpoint.
	got, err := c.resolveIssueId(ctx, "KONTAINER-BACKEND-4JH")
	if err != nil || got != "60741" {
		t.Fatalf("short id = %q, %v; want 60741", got, err)
	}
	if !s.requested("/organizations/konform/shortids/KONTAINER-BACKEND-4JH/") {
		t.Errorf("shortids endpoint not used: %v", s.paths())
	}

	// Sentry versions differ on where the id sits in that response.
	if got, err := c.resolveIssueId(ctx, "GROUP-ONLY"); err != nil || got != "777" {
		t.Errorf("nested group id = %q, %v; want 777", got, err)
	}

	// An unknown short id explains itself rather than surfacing a bare 404.
	if _, err := c.resolveIssueId(ctx, "NOPE-XYZ"); err == nil || !strings.Contains(err.Error(), "NOPE-XYZ") {
		t.Errorf("unknown short id error = %v, want it to name the input", err)
	}
	if _, err := c.resolveIssueId(ctx, ""); err == nil {
		t.Error("an empty reference should be rejected")
	}
	if _, err := c.resolveIssueId(ctx, "has spaces"); err == nil || !strings.Contains(err.Error(), "short ID") {
		t.Errorf("garbage error = %v, want it to name the accepted forms", err)
	}
}

func TestResolveEventPicksTheRightEndpoint(t *testing.T) {
	c, _ := newStub(t, map[string]string{
		"/organizations/konform/shortids/KONTAINER-BACKEND-4JH/": `{"groupId":"60741"}`,
		"/projects/konform/kontainer-backend/issues/":            `[{"id":"99"}]`,
	})
	ctx := context.Background()

	cases := []struct {
		name                       string
		project, eventId, issueRef string
		want                       string
	}{
		{"issue ref alone reads the latest event", "", "", "60741",
			"organizations/konform/issues/60741/events/latest/"},
		{"issue ref with an explicit event", "", "abc123", "60741",
			"organizations/konform/issues/60741/events/abc123/"},
		{"short id needs no project", "", "", "KONTAINER-BACKEND-4JH",
			"organizations/konform/issues/60741/events/latest/"},
		{"eventId=latest is explicit", "", "LATEST", "60741",
			"organizations/konform/issues/60741/events/latest/"},
		{"project plus event id", "kontainer-backend", "abc123", "",
			"projects/konform/kontainer-backend/events/abc123/"},
		{"project alone falls through to its newest issue", "kontainer-backend", "", "",
			"organizations/konform/issues/99/events/latest/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := c.resolveEvent(ctx, tc.project, tc.eventId, tc.issueRef)
			if err != nil {
				t.Fatalf("resolveEvent: %v", err)
			}
			if ref.endpoint != tc.want {
				t.Errorf("endpoint = %q, want %q", ref.endpoint, tc.want)
			}
		})
	}

	// With nothing to go on, the error names both routes and the projects.
	_, err := c.resolveEvent(ctx, "", "", "")
	if err == nil {
		t.Fatal("expected an error with no references at all")
	}
	for _, want := range []string{"issueIdOrUrl", "projectSlug"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestResolveAssignee(t *testing.T) {
	members := `[
		{"role":"member","email":"abs@kontainer.com","user":{"username":"abs","email":"abs@kontainer.com","name":"Alexander Stage"}},
		{"role":"member","email":"alex@kontainer.com","user":{"username":"alexk","email":"alex@kontainer.com","name":"Alex Kringle"}}
	]`
	c, _ := newStub(t, map[string]string{"/organizations/konform/members/": members})
	ctx := context.Background()

	// Empty means unassign, and a team actor is passed through untouched.
	for in, want := range map[string]string{"": "", "team:backend": "team:backend"} {
		if got, err := c.resolveAssignee(ctx, in); err != nil || got != want {
			t.Errorf("resolveAssignee(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	// An exact username or email hit is unambiguous by definition.
	for _, in := range []string{"abs", "ABS", "abs@kontainer.com"} {
		if got, err := c.resolveAssignee(ctx, in); err != nil || got != "abs" {
			t.Errorf("resolveAssignee(%q) = %q, %v; want abs", in, got, err)
		}
	}

	// An ambiguous name is an error that lists the usernames to choose from,
	// rather than a coin flip that silently assigns the wrong person.
	_, err := c.resolveAssignee(ctx, "Al")
	if err == nil {
		t.Fatal("an ambiguous name should be an error")
	}
	for _, want := range []string{"abs", "alexk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error %q should offer %q", err, want)
		}
	}

	// A single fuzzy match resolves to its username.
	single, _ := newStub(t, map[string]string{
		"/organizations/konform/members/": `[{"user":{"username":"abs","email":"abs@kontainer.com","name":"Alexander Stage"}}]`,
	})
	if got, err := single.resolveAssignee(ctx, "Alexander Stage"); err != nil || got != "abs" {
		t.Errorf("single match = %q, %v; want abs", got, err)
	}

	// Nothing matched: pass the value through rather than block, because
	// Sentry's member search does not index every valid actor string.
	empty, _ := newStub(t, map[string]string{"/organizations/konform/members/": `[]`})
	if got, err := empty.resolveAssignee(ctx, "someone"); err != nil || got != "someone" {
		t.Errorf("no match = %q, %v; want it passed through", got, err)
	}
}

func TestAssignIssueVerifiesTheReadBack(t *testing.T) {
	ctx := context.Background()

	// Sentry accepts an actor it cannot resolve and returns the issue still
	// unassigned. That must be an error, not a confirmation.
	silent, _ := newStub(t, map[string]string{"/issues/60741/": `{"id":"60741","assignedTo":null}`})
	if _, err := silent.assignIssue(ctx, "60741", "ghost"); err == nil {
		t.Fatal("an assignment that did not stick should be an error")
	} else if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "sentry_search") {
		t.Errorf("error %q should name the value and how to look one up", err)
	}

	ok, _ := newStub(t, map[string]string{
		"/issues/60741/": `{"id":"60741","assignedTo":{"username":"abs","name":"Alexander Stage"}}`,
	})
	msg, err := ok.assignIssue(ctx, "60741", "abs")
	if err != nil || !strings.Contains(msg, "abs") {
		t.Errorf("successful assign = %q, %v", msg, err)
	}

	// Unassigning is confirmed only when the issue really came back empty.
	cleared, _ := newStub(t, map[string]string{"/issues/60741/": `{"id":"60741","assignedTo":null}`})
	if msg, err := cleared.assignIssue(ctx, "60741", ""); err != nil || !strings.Contains(msg, "unassigned") {
		t.Errorf("unassign = %q, %v", msg, err)
	}
	stuck, _ := newStub(t, map[string]string{
		"/issues/60741/": `{"id":"60741","assignedTo":{"username":"abs"}}`,
	})
	if _, err := stuck.assignIssue(ctx, "60741", ""); err == nil {
		t.Error("an unassign that left an assignee should be an error")
	}
}

// TestStackFramesFromShortId is the end-to-end version of the ergonomics fix:
// the identifier the server prints, straight into the trace tool, in one call
// and with no project slug.
func TestStackFramesFromShortId(t *testing.T) {
	event := map[string]any{
		"eventID": "abc123",
		"entries": []any{map[string]any{
			"type": "exception",
			"data": map[string]any{"values": []any{map[string]any{
				"stacktrace": map[string]any{"frames": []any{
					map[string]any{"function": "handle", "filename": "app/Job.php", "lineNo": 12, "in_app": false},
					map[string]any{"function": "show", "filename": "app/Asset.php", "lineNo": 42, "in_app": true},
				}},
			}}},
		}},
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	c, s := newStub(t, map[string]string{
		"/organizations/konform/shortids/KONTAINER-BACKEND-4JH/": `{"groupId":"60741"}`,
		"/organizations/konform/issues/60741/events/latest/":     string(raw),
	})

	res, err := c.getStackFrames(context.Background(), "", "", "KONTAINER-BACKEND-4JH", false, 50)
	if err != nil {
		t.Fatalf("getStackFrames: %v", err)
	}
	out := res.Content[0].Text
	for _, want := range []string{"frames[2]{", "show", "app/Asset.php"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// _full must name the endpoint that was actually used, since that is what
	// a follow-up sentry_raw_api call is handed.
	if !strings.Contains(out, "organizations/konform/issues/60741/events/latest/") {
		t.Errorf("_full does not name the resolved endpoint:\n%s", out)
	}
	if !s.requested("/organizations/konform/issues/60741/events/latest/") {
		t.Errorf("issue-scoped event endpoint not used: %v", s.paths())
	}
}

func TestListIssuesGoesOrgWideWithoutAProject(t *testing.T) {
	issues := `[{"id":"1","shortId":"A-1","title":"boom","project":{"slug":"web"}}]`
	c, s := newStub(t, map[string]string{
		"/organizations/konform/issues/":              issues,
		"/projects/konform/kontainer-backend/issues/": issues,
	})
	ctx := context.Background()

	if _, err := c.listIssues(ctx, "", "is:unresolved", "", 25, ""); err != nil {
		t.Fatalf("org-wide search: %v", err)
	}
	if !s.requested("/organizations/konform/issues/") {
		t.Errorf("no project should search org-wide, but hit %v", s.paths())
	}

	if _, err := c.listIssues(ctx, "kontainer-backend", "", "", 25, ""); err != nil {
		t.Fatalf("project search: %v", err)
	}
	if !s.requested("/projects/konform/kontainer-backend/issues/") {
		t.Errorf("a named project should scope the search, but hit %v", s.paths())
	}
}

func TestProjectHintNamesKnownProjects(t *testing.T) {
	c, _ := newStub(t, map[string]string{
		"/organizations/konform/projects/": `[{"slug":"kontainer-backend"},{"slug":"internal"}]`,
	})
	hint := c.projectHint(context.Background())
	for _, want := range []string{"kontainer-backend", "internal"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q should list %q", hint, want)
		}
	}
	// An unreachable project list must not break the error it decorates.
	broken, _ := newStub(t, map[string]string{})
	if h := broken.projectHint(context.Background()); h != "" {
		t.Errorf("unavailable project list should yield no hint, got %q", h)
	}
}
