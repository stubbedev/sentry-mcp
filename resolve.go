package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ── Resolving what the caller actually has ───────────────────────────────────
//
// Each helper here closes a gap between the identifier a caller is holding and
// the one an endpoint wants. The server prints short ids in sentry_get_dev_context
// and returns them on every issue row, so a tool that accepts only numeric ids
// makes the caller pay a round trip to translate the server's own output. The
// issue-scoped event endpoints need no project slug, so requiring one turns
// "issue 60741, show me the trace" into three calls. And an unknown assignee
// username is accepted by Sentry and silently leaves the issue unassigned,
// which is a failure the server can catch instead of warning about.

// resolveIssueId turns any issue reference a caller might hold into the numeric
// id the /issues/ endpoints take: a numeric id, an issue URL, or a short id
// like KONTAINER-BACKEND-4JH.
func (c *SentryClient) resolveIssueId(ctx context.Context, input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", fmt.Errorf("An issue reference is required: a numeric ID, a short ID like PROJECT-ABC, or an issue URL.")
	}
	if id := extractIssueId(trimmed); id != "" {
		return id, nil
	}
	candidate := shortIdFromInput(trimmed)
	if candidate == "" {
		return "", fmt.Errorf("Could not read an issue reference from %q. Pass a numeric issue ID, a short ID like PROJECT-ABC, or an issue URL.", input)
	}
	resp, err := c.request(ctx, "GET", "/organizations/"+c.OrgSlug+"/shortids/"+url.PathEscape(candidate)+"/", nil, nil)
	if err != nil {
		return "", fmt.Errorf("Could not resolve %q as a short ID: %v", candidate, err)
	}
	m := toObject(resp.data)
	for _, k := range []string{"groupId", "issueId"} {
		if s := asString(m, k); s != "" {
			return s, nil
		}
	}
	for _, k := range []string{"group", "issue"} {
		if s := asString(toObject(m[k]), "id"); s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("Sentry resolved short ID %q but returned no issue ID.", candidate)
}

// shortIdFromInput extracts a candidate short id from a bare reference or a
// URL. Sentry links a short id either as a path segment or as a ?query=, so
// both are checked before giving up.
func shortIdFromInput(input string) string {
	s := strings.TrimSpace(input)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		s = ""
		parts := splitNonEmpty(u.Path, "/")
		for i, p := range parts {
			if p == "issues" && i+1 < len(parts) {
				s = parts[i+1]
				break
			}
		}
		if s == "" {
			s = u.Query().Get("query")
		}
	}
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || strings.ContainsAny(s, " \t/?&#") {
		return ""
	}
	return s
}

// eventRef is a resolved event fetch: the raw-API endpoint that returns the
// event, and the project slug when the caller supplied one.
type eventRef struct {
	endpoint string
	project  string
}

// resolveEvent works out which endpoint returns the event the caller means,
// from whichever combination of references they have:
//
//	issueRef (+ optional eventId)   → the issue's events, no project needed
//	projectSlug + eventId           → that event in that project
//	projectSlug alone               → the project's most recent issue's latest event
//
// eventId may be "latest" (or empty) anywhere an id is accepted.
func (c *SentryClient) resolveEvent(ctx context.Context, projectSlug, eventId, issueRef string) (eventRef, error) {
	eventId = strings.TrimSpace(eventId)
	latest := eventId == "" || strings.EqualFold(eventId, "latest")

	if strings.TrimSpace(issueRef) != "" {
		id, err := c.resolveIssueId(ctx, issueRef)
		if err != nil {
			return eventRef{}, err
		}
		which := eventId
		if latest {
			which = "latest"
		}
		return eventRef{
			endpoint: "organizations/" + c.OrgSlug + "/issues/" + id + "/events/" + which + "/",
			project:  projectSlug,
		}, nil
	}

	if projectSlug == "" {
		return eventRef{}, fmt.Errorf("Pass issueIdOrUrl (an issue ID, short ID, or URL), or projectSlug with eventId.%s", c.projectHint(ctx))
	}

	if !latest {
		return eventRef{
			endpoint: "projects/" + c.OrgSlug + "/" + projectSlug + "/events/" + eventId + "/",
			project:  projectSlug,
		}, nil
	}

	// A project with no event named: fall through to its most recent issue.
	resp, err := c.request(ctx, "GET", "/projects/"+c.OrgSlug+"/"+projectSlug+"/issues/", map[string]any{"limit": 1}, nil)
	if err != nil {
		return eventRef{}, err
	}
	issues := toArray(resp.data)
	if len(issues) == 0 {
		return eventRef{}, fmt.Errorf("No issues found in %q, so there is no latest event to read.", projectSlug)
	}
	id := asString(toObject(issues[0]), "id")
	if id == "" {
		return eventRef{}, fmt.Errorf("Sentry returned an issue with no ID for %q.", projectSlug)
	}
	return eventRef{
		endpoint: "organizations/" + c.OrgSlug + "/issues/" + id + "/events/latest/",
		project:  projectSlug,
	}, nil
}

// fetch retrieves the resolved event.
func (c *SentryClient) fetchEvent(ctx context.Context, ref eventRef) (map[string]any, error) {
	resp, err := c.request(ctx, "GET", "/"+ref.endpoint, nil, nil)
	if err != nil {
		return nil, err
	}
	return toObject(resp.data), nil
}

// projectHint lists the org's project slugs for an error message. It runs only
// on a failure path, so the extra request costs nothing in normal use.
func (c *SentryClient) projectHint(ctx context.Context) string {
	projects := c.fetchProjects(ctx, 100)
	slugs := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.slug != "" {
			slugs = append(slugs, p.slug)
		}
	}
	if len(slugs) == 0 {
		return ""
	}
	const show = 25
	if len(slugs) > show {
		return fmt.Sprintf(" Known projects: %s, … and %d more.", strings.Join(slugs[:show], ", "), len(slugs)-show)
	}
	return " Known projects: " + strings.Join(slugs, ", ") + "."
}

// ── Assignees ────────────────────────────────────────────────────────────────

// memberCandidate is one org member matched while resolving an assignee.
type memberCandidate struct {
	username, email, name string
}

func (m memberCandidate) label() string {
	switch {
	case m.name != "" && m.email != "":
		return fmt.Sprintf("%s <%s> → %s", m.name, m.email, m.username)
	case m.email != "":
		return fmt.Sprintf("%s → %s", m.email, m.username)
	default:
		return m.username
	}
}

// resolveAssignee turns whatever the caller has — a username, an email, a real
// name — into the actor string Sentry's assignedTo field accepts.
//
// An unresolvable value is passed through rather than rejected: Sentry's member
// search matches on email and name, so a valid username that the search does
// not return must still reach the API. assignIssue verifies the read-back, so a
// value that does not stick is reported there rather than silently accepted.
func (c *SentryClient) resolveAssignee(ctx context.Context, input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", nil // unassign
	}
	if strings.HasPrefix(s, "team:") {
		return s, nil
	}

	resp, err := c.request(ctx, "GET", "/organizations/"+c.OrgSlug+"/members/",
		map[string]any{"query": s, "per_page": 25}, nil)
	if err != nil {
		return s, nil // lookup unavailable; let the assignment attempt speak
	}

	needle := strings.ToLower(s)
	var candidates []memberCandidate
	for _, m := range toArray(resp.data) {
		member := toObject(m)
		user := toObject(member["user"])
		cand := memberCandidate{
			username: asString(user, "username"),
			email:    firstNonEmpty(asString(user, "email"), asString(member, "email")),
			name:     firstNonEmpty(asString(user, "name"), asString(member, "name")),
		}
		if cand.username == "" {
			cand.username = cand.email
		}
		if cand.username == "" {
			continue
		}
		// An exact hit on the username or email is unambiguous by definition.
		if strings.EqualFold(cand.username, needle) || strings.EqualFold(cand.email, needle) {
			return cand.username, nil
		}
		candidates = append(candidates, cand)
	}

	switch len(candidates) {
	case 0:
		return s, nil
	case 1:
		return candidates[0].username, nil
	default:
		labels := make([]string, 0, len(candidates))
		for _, cand := range candidates {
			labels = append(labels, cand.label())
		}
		return "", fmt.Errorf("%q matches %d members — pass one of these usernames instead: %s",
			s, len(candidates), strings.Join(labels, "; "))
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
