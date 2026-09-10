package main

import (
	"encoding/json"
	"strings"
)

// ── Compact projection ───────────────────────────────────────────────────────
//
// TOON collapses an array of objects into a table — one header line, then one
// line per row — but only when every element is a flat object carrying the same
// key set. A single nested object, or a key present in some rows and absent in
// others, drops the whole array back to expanded per-field blocks: on a
// 25-issue list that is ~2.5x the characters for identical information. So
// everything here exists to keep rows flat and uniform.
//
// The other half is not repeating what the reader can already derive. A
// permalink is a pure function of an issue id; advice prose is the same
// sentence on every call. Both are stated once — the link templates and the
// response conventions live in buildInstructions — and dropped from the rows.
// Identifiers stay, renderings of identifiers go, so anything the model needs
// to cross-reference a row against another call is still in hand.

// hoistMinRows is the row count from which pulling a shared column out into
// "_all" pays for the lines the shared block itself costs.
const hoistMinRows = 3

// identityCols are the join keys a row is looked up by. They are never hoisted
// out of a row even when they happen to be constant, because a row that cannot
// be addressed cannot be cross-referenced.
var identityCols = []string{"id", "shortId", "eventID", "eventId", "slug", "username"}

// isScalar reports whether v fits in a single TOON table cell.
func isScalar(v any) bool {
	switch v.(type) {
	case nil, bool, string, float64, int, int64, json.Number:
		return true
	}
	return false
}

// tabular normalizes rows into a uniform table: any column that is null or
// absent in every row is dropped outright, and every surviving column is
// present on every row.
//
// Both halves matter. Dropping a null column pays only when it is dropped
// everywhere — omitting nulls per row instead leaves ragged key sets, which
// de-tabularizes the array and costs *more* than emitting the nulls (measured
// at +42% on a 40-frame trace). Deciding the column set per response is what
// makes it safe for a projection to delete a redundant key from one row.
func tabular(rows []map[string]any) []any {
	live := map[string]bool{}
	for _, r := range rows {
		for k, v := range r {
			if v != nil && isScalar(v) {
				live[k] = true
			}
		}
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		row := make(map[string]any, len(live))
		for k := range live {
			v, ok := r[k]
			if !ok || !isScalar(v) {
				v = nil
			}
			row[k] = v
		}
		out = append(out, row)
	}
	return out
}

// hoistConstants moves every column that carries the same value in all rows out
// of the rows and into a shared map, so a 25-issue list states its project and
// platform once instead of 25 times.
func hoistConstants(rows []map[string]any, minRows int) (map[string]any, []map[string]any) {
	if len(rows) < minRows || len(rows) < 2 {
		return nil, rows
	}
	common := map[string]any{}
	for k, v := range rows[0] {
		// Guarding on isScalar also keeps the comparison below safe: an
		// uncomparable dynamic type on both sides would panic.
		if !isScalar(v) {
			continue
		}
		same := true
		for _, r := range rows[1:] {
			if o, ok := r[k]; !ok || o != v {
				same = false
				break
			}
		}
		// A column that is nil in every row is left in place so tabular can
		// delete it outright: hoisting it would only move a useless null into
		// the shared block.
		if same && v != nil {
			common[k] = v
		}
	}
	for _, k := range identityCols {
		delete(common, k)
	}
	if len(common) == 0 {
		return nil, rows
	}
	trimmed := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		t := make(map[string]any, len(r))
		for k, v := range r {
			if _, shared := common[k]; !shared {
				t[k] = v
			}
		}
		trimmed = append(trimmed, t)
	}
	return common, trimmed
}

// dropMirror deletes col from every row when it only ever repeats other — a
// Sentry project or team name is usually its own slug, and a member's email is
// usually their username. Like tabular, the decision is made across the whole
// response, so the surviving column set stays uniform.
func dropMirror(rows []map[string]any, col, other string) {
	for _, r := range rows {
		a, b := r[col], r[other]
		if !isScalar(a) || !isScalar(b) || a != b {
			return
		}
	}
	for _, r := range rows {
		delete(r, col)
	}
}

// compactTable is the whole pipeline for a list response: hoist the columns
// every row agrees on, make the rest uniform, and fold in the top-level fields
// a caller needs (counts, cursors, refs).
func compactTable(key string, rows []map[string]any, extra map[string]any) map[string]any {
	common, trimmed := hoistConstants(rows, hoistMinRows)
	out := map[string]any{key: tabular(trimmed), "count": len(rows)}
	if len(common) > 0 {
		out["_all"] = common
	}
	for k, v := range extra {
		if v != nil && v != "" {
			out[k] = v
		}
	}
	return out
}

// secs trims an ISO-8601 timestamp to whole seconds. Sentry sends microsecond
// precision that no triage decision reads and that tokenizes badly. The
// timestamp stays absolute rather than becoming a relative "2h ago", so it
// remains comparable across calls and against logs.
func secs(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	for i := dot + 1; i < len(s); i++ {
		if c := s[i]; c < '0' || c > '9' {
			return s[:dot] + s[i:] // keep the timezone suffix
		}
	}
	return s[:dot]
}

// pick returns the first present, non-empty value among keys — the flat scalar
// standing in for a nested object (assignedTo → a name, project → a slug).
func pick(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil && v != "" {
			return v
		}
	}
	return nil
}

// ── Issues ───────────────────────────────────────────────────────────────────

// issueRow projects a Sentry issue into one flat table row: nested project,
// metadata and assignedTo objects become scalar columns, and permalink is
// dropped because id rebuilds it.
func issueRow(issue map[string]any) map[string]any {
	meta := toObject(issue["metadata"])
	row := map[string]any{
		"id":        issue["id"],
		"shortId":   issue["shortId"],
		"title":     issue["title"],
		"culprit":   issue["culprit"],
		"level":     issue["level"],
		"status":    issue["status"],
		"substatus": issue["substatus"],
		"type":      issue["type"],
		"platform":  issue["platform"],
		"logger":    issue["logger"],
		"count":     issue["count"],
		"userCount": issue["userCount"],
		"firstSeen": secs(issue["firstSeen"]),
		"lastSeen":  secs(issue["lastSeen"]),
		"project":   pick(toObject(issue["project"]), "slug", "name"),
		"assignee":  pick(toObject(issue["assignedTo"]), "name", "username", "email"),
		"metaType":  meta["type"],
		"metaValue": meta["value"],
	}
	// Sentry's title is normally "metaType: metaValue". When it is, the parts
	// are the title said twice more. Deleting them per row is safe because
	// tabular decides the final column set across the whole response: if every
	// row is redundant the columns vanish, and if only some are the rest get an
	// explicit null and the table survives.
	if title, _ := row["title"].(string); title != "" {
		t, _ := row["metaType"].(string)
		v, _ := row["metaValue"].(string)
		if t != "" && (title == t+": "+v || (v == "" && title == t)) {
			delete(row, "metaType")
			delete(row, "metaValue")
		}
	}
	return row
}

// ── Events ───────────────────────────────────────────────────────────────────

// frameRow projects one stack frame into a flat row. absPath is dropped when
// filename already carries it, and the native-only columns (module, package,
// addresses) are listed generously here because tabular deletes whichever ones
// this platform never populates.
func frameRow(frame map[string]any) map[string]any {
	filename := pick(frame, "filename", "absPath")
	row := map[string]any{
		"function":        pick(frame, "function", "rawFunction"),
		"filename":        filename,
		"lineNo":          frame["lineNo"],
		"colNo":           frame["colNo"],
		"inApp":           frame["inApp"],
		"module":          frame["module"],
		"package":         frame["package"],
		"instructionAddr": frame["instructionAddr"],
		"symbolAddr":      frame["symbolAddr"],
	}
	if row["inApp"] == nil {
		row["inApp"] = frame["in_app"]
	}
	if abs, ok := frame["absPath"].(string); ok {
		if name, ok := filename.(string); !ok || !strings.HasSuffix(abs, name) {
			row["absPath"] = abs
		}
	}
	return row
}

// frameSource is the source-context window for one frame, rendered as
// "lineNo| text" lines. Sentry attaches this to most frames; carrying it on all
// of them is what makes a stack trace dominate an event payload, so callers
// attach it to the one frame a reader starts from.
func frameSource(frame map[string]any) map[string]any {
	ctxLines := toArray(frame["context"])
	if len(ctxLines) == 0 {
		return nil
	}
	lines := make([]string, 0, len(ctxLines))
	for _, l := range ctxLines {
		pair := toArray(l)
		if len(pair) < 2 {
			continue
		}
		lines = append(lines, strings.TrimRight(toQueryString(pair[0])+"| "+toQueryString(pair[1]), " "))
	}
	if len(lines) == 0 {
		return nil
	}
	return map[string]any{
		"filename": pick(frame, "filename", "absPath"),
		"lineNo":   frame["lineNo"],
		"lines":    lines,
	}
}

// deepestInApp returns the last in-app frame, which is where a reader looks
// first: the innermost line of the caller's own code. It falls back to the last
// frame when nothing is marked in-app.
func deepestInApp(frames []map[string]any) map[string]any {
	var fallback map[string]any
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if f == nil {
			continue
		}
		if fallback == nil {
			fallback = f
		}
		inApp, _ := f["inApp"].(bool)
		if !inApp {
			inApp, _ = f["in_app"].(bool)
		}
		if inApp {
			return f
		}
	}
	return fallback
}

// frameObjects drops the nils out of a raw frame array so the frame helpers can
// take a single concrete type.
func frameObjects(frames []any) []map[string]any {
	out := make([]map[string]any, 0, len(frames))
	for _, f := range frames {
		if fr := toObject(f); fr != nil {
			out = append(out, fr)
		}
	}
	return out
}

// exceptionValues projects an exception entry's values: a flat frames table per
// exception, plus the source window for the frame that matters. maxFrames caps
// the trace from the bottom, keeping the frames nearest the throw.
func exceptionValues(values []any, maxFrames int) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		exc := toObject(v)
		if exc == nil {
			continue
		}
		item := map[string]any{
			"type":      exc["type"],
			"value":     exc["value"],
			"mechanism": pick(toObject(exc["mechanism"]), "type"),
		}
		frames := frameObjects(toArray(toObject(exc["stacktrace"])["frames"]))
		if len(frames) > 0 {
			if src := frameSource(deepestInApp(frames)); src != nil {
				item["source"] = src
			}
			if maxFrames > 0 && len(frames) > maxFrames {
				item["frames_omitted"] = len(frames) - maxFrames
				frames = frames[len(frames)-maxFrames:]
			}
			rows := make([]map[string]any, 0, len(frames))
			for _, f := range frames {
				rows = append(rows, frameRow(f))
			}
			item["frames"] = tabular(rows)
		}
		out = append(out, item)
	}
	return out
}

// breadcrumbRows flattens breadcrumbs into a table, dropping each crumb's
// nested data bag — the ref holds it when a crumb turns out to matter.
func breadcrumbRows(values []any) []any {
	rows := make([]map[string]any, 0, len(values))
	for _, v := range values {
		c := toObject(v)
		if c == nil {
			continue
		}
		rows = append(rows, map[string]any{
			"timestamp": secs(c["timestamp"]),
			"type":      c["type"],
			"category":  c["category"],
			"level":     c["level"],
			"message":   c["message"],
		})
	}
	return tabular(rows)
}

// maxEventFrames caps stack traces inside a projected event entry. A reader
// wanting the whole trace has sentry_stack_frames or the payload ref.
const maxEventFrames = 12

// maxBreadcrumbs caps how many of the most recent crumbs an entry carries.
const maxBreadcrumbs = 10

// maxIssueEventEntries caps how many entries of an issue's latest event ride
// along on sentry_get_issue, which is an overview rather than a drill-down.
const maxIssueEventEntries = 3

// eventEntry projects one event entry down to what a reader acts on. Entry
// types with no projection collapse to a marker: available_types on the
// response lists them, and the payload ref can produce them in full.
func eventEntry(entry map[string]any, maxFrames int) map[string]any {
	data := toObject(entry["data"])
	switch asString(entry, "type") {
	case "exception":
		return map[string]any{
			"type":   "exception",
			"values": exceptionValues(toArray(data["values"]), maxFrames),
		}
	case "message":
		return map[string]any{
			"type":    "message",
			"message": pick(data, "formatted", "message"),
		}
	case "breadcrumbs":
		values := toArray(data["values"])
		out := map[string]any{"type": "breadcrumbs"}
		if len(values) > maxBreadcrumbs {
			out["omitted"] = len(values) - maxBreadcrumbs
			values = values[len(values)-maxBreadcrumbs:]
		}
		out["values"] = breadcrumbRows(values)
		return out
	case "request":
		// Headers and cookies are both bulky and the likeliest place for a
		// token to sit, so they stay behind the ref.
		return map[string]any{
			"type":   "request",
			"method": data["method"],
			"url":    data["url"],
			"query":  data["query"],
		}
	default:
		return map[string]any{"type": entry["type"], "_omitted": true}
	}
}
