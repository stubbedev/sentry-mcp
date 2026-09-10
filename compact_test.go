package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func toonOf(t *testing.T, v any) string {
	t.Helper()
	return renderString(ctxWithFormat(context.Background(), "toon"), v)
}

// assertAllScalar is the invariant the whole projection rests on: a row that
// holds a nested value de-tabularizes the array it sits in.
func assertAllScalar(t *testing.T, label string, row map[string]any) {
	t.Helper()
	for k, v := range row {
		if !isScalar(v) {
			t.Errorf("%s: column %q holds a non-scalar %T — this de-tabularizes the whole array", label, k, v)
		}
	}
}

func TestTabularDropsAllNullColumnsAndFillsTheRest(t *testing.T) {
	rows := []map[string]any{
		{"a": 1, "dead": nil, "sometimes": nil},
		{"a": 2, "dead": nil, "sometimes": "x"},
		{"a": 3, "dead": nil},
	}
	out := tabular(rows)
	if len(out) != 3 {
		t.Fatalf("row count = %d, want 3", len(out))
	}
	for i, r := range out {
		row := r.(map[string]any)
		if _, ok := row["dead"]; ok {
			t.Errorf("row %d kept an all-null column", i)
		}
		// "sometimes" is set in exactly one row, so every row must carry it —
		// a ragged key set is what costs more than emitting the nulls.
		if _, ok := row["sometimes"]; !ok {
			t.Errorf("row %d is missing a column another row has", i)
		}
	}
}

func TestHoistConstants(t *testing.T) {
	rows := []map[string]any{
		{"id": "1", "project": "web", "level": "error"},
		{"id": "2", "project": "web", "level": "warning"},
		{"id": "3", "project": "web", "level": "error"},
	}
	common, trimmed := hoistConstants(rows, hoistMinRows)
	if common["project"] != "web" {
		t.Errorf("project not hoisted: %#v", common)
	}
	if _, ok := common["level"]; ok {
		t.Error("level varies between rows and must not be hoisted")
	}
	for _, r := range trimmed {
		if _, ok := r["project"]; ok {
			t.Error("hoisted column left behind on the row")
		}
		if r["id"] == nil {
			t.Error("id dropped from a row — the join key must survive")
		}
	}

	// An identity column is never hoisted, even when every row agrees: a row
	// that cannot be addressed cannot be cross-referenced.
	same := []map[string]any{{"id": "1", "x": 1}, {"id": "1", "x": 1}, {"id": "1", "x": 1}}
	common, _ = hoistConstants(same, hoistMinRows)
	if _, ok := common["id"]; ok {
		t.Error("id was hoisted out of the rows")
	}

	// A column that is null in every row must not be hoisted: it belongs in
	// tabular's hands, which drops it entirely rather than parking a null in
	// the shared block.
	withDead := []map[string]any{
		{"id": "1", "assignee": nil}, {"id": "2", "assignee": nil}, {"id": "3", "assignee": nil},
	}
	common, trimmed = hoistConstants(withDead, hoistMinRows)
	if _, ok := common["assignee"]; ok {
		t.Error("an all-null column was hoisted instead of dropped")
	}
	for _, r := range tabular(trimmed) {
		if _, ok := r.(map[string]any)["assignee"]; ok {
			t.Error("an all-null column survived tabular")
		}
	}

	// Below the threshold the shared block costs more than it saves.
	if c, _ := hoistConstants(rows[:2], hoistMinRows); c != nil {
		t.Errorf("hoisted below hoistMinRows: %#v", c)
	}
}

func TestDropMirror(t *testing.T) {
	mirrored := []map[string]any{{"slug": "web", "name": "web"}, {"slug": "api", "name": "api"}}
	dropMirror(mirrored, "name", "slug")
	for _, r := range mirrored {
		if _, ok := r["name"]; ok {
			t.Error("name mirrors slug in every row and should be dropped")
		}
	}
	// One row differing keeps the column for all of them, so the column set
	// stays uniform.
	partial := []map[string]any{{"slug": "web", "name": "web"}, {"slug": "api", "name": "API Gateway"}}
	dropMirror(partial, "name", "slug")
	for i, r := range partial {
		if _, ok := r["name"]; !ok {
			t.Errorf("row %d lost a column that differs elsewhere", i)
		}
	}
}

func TestSecs(t *testing.T) {
	cases := map[string]string{
		"2026-08-14T09:12:44.183726Z":      "2026-08-14T09:12:44Z",
		"2026-08-14T09:12:44Z":             "2026-08-14T09:12:44Z",
		"2026-08-14T09:12:44.183726+02:00": "2026-08-14T09:12:44+02:00",
		"2026-08-14T09:12:44.183726":       "2026-08-14T09:12:44",
		"not a timestamp":                  "not a timestamp",
	}
	for in, want := range cases {
		if got := secs(in); got != want {
			t.Errorf("secs(%q) = %v, want %q", in, got, want)
		}
	}
	// A non-string passes through untouched rather than being stringified.
	if got := secs(float64(17)); got != float64(17) {
		t.Errorf("secs(number) = %v, want it unchanged", got)
	}
}

var issueTitles = []string{
	"TypeError: Cannot read property 'id' of undefined",
	"QueryException: SQLSTATE[42S02] base table not found",
	"ValidationException: The given data was invalid",
}

func rawIssue(i int) map[string]any {
	return map[string]any{
		"id":        fmt.Sprintf("60741%d", i),
		"shortId":   fmt.Sprintf("KONTAINER-BACKEND-%dJH", i),
		"title":     issueTitles[i%len(issueTitles)],
		"culprit":   fmt.Sprintf("app/Http/Controllers/AssetController.php in handler%d", i),
		"level":     []string{"error", "warning", "fatal"}[i%3],
		"status":    "unresolved",
		"type":      "error",
		"platform":  "php",
		"count":     fmt.Sprintf("%d", 100+i*37),
		"userCount": float64(i * 3),
		"firstSeen": fmt.Sprintf("2026-08-%02dT09:12:44.183726Z", 1+i%28),
		"lastSeen":  "2026-09-10T18:02:11.552091Z",
		"permalink": fmt.Sprintf("https://sentry.konform.com/organizations/konform/issues/60741%d/", i),
		"project":   map[string]any{"id": "12", "slug": "kontainer-backend", "platform": "php-laravel"},
		"metadata":  map[string]any{"type": metaOf(i), "value": valueOf(i)},
		"assignedTo": map[string]any{
			"name": "Alexander", "username": "abs", "email": "abs@kontainer.com",
		},
	}
}

// metaOf and valueOf split a title the way Sentry's metadata does, so the
// fixture exercises the redundancy check rather than dodging it.
func metaOf(i int) string {
	title := issueTitles[i%len(issueTitles)]
	return strings.SplitN(title, ": ", 2)[0]
}

func valueOf(i int) string {
	return strings.SplitN(issueTitles[i%len(issueTitles)], ": ", 2)[1]
}

func TestIssueRowFlattensAndDropsDerivable(t *testing.T) {
	row := issueRow(rawIssue(0))
	assertAllScalar(t, "issueRow", row)

	if row["project"] != "kontainer-backend" {
		t.Errorf("project = %v, want the slug as a scalar", row["project"])
	}
	if row["assignee"] != "Alexander" {
		t.Errorf("assignee = %v, want a scalar name", row["assignee"])
	}
	if _, ok := row["permalink"]; ok {
		t.Error("permalink is a pure function of id and must not be returned")
	}
	if seen, _ := row["firstSeen"].(string); !strings.HasSuffix(seen, ":44Z") || strings.Contains(seen, ".") {
		t.Errorf("firstSeen = %v, want second precision with no fractional part", row["firstSeen"])
	}
	// title is exactly "metaType: metaValue" here, so the parts are redundant.
	if _, ok := row["metaType"]; ok {
		t.Error("metaType repeats the title and should be dropped")
	}
	// The join keys must always survive, since they are what makes a row
	// cross-referenceable against another call.
	for _, k := range []string{"id", "shortId"} {
		if row[k] == nil {
			t.Errorf("join key %q missing from the row", k)
		}
	}

	// A title that is not the metadata restated keeps the parts.
	custom := rawIssue(1)
	custom["title"] = "Asset lookup exploded"
	if _, ok := issueRow(custom)["metaValue"]; !ok {
		t.Error("metaValue should survive when the title does not already say it")
	}
}

// TestIssueListStaysTabular is the regression guard for the compaction itself.
// A nested value or a ragged key set silently drops TOON back to expanded
// per-field blocks — the same data at ~2.5x the characters — and no other test
// in the suite would notice. The baseline here is the shape this projection
// replaced: raw Sentry issues with nested project/metadata/assignedTo.
func TestIssueListStaysTabular(t *testing.T) {
	naive := make([]any, 0, 25)
	rows := make([]map[string]any, 0, 25)
	for i := range 25 {
		naive = append(naive, rawIssue(i))
		rows = append(rows, issueRow(rawIssue(i)))
	}

	before := toonOf(t, map[string]any{"issues": naive, "count": 25})
	after := toonOf(t, compactTable("issues", rows, map[string]any{"next_cursor": "0:100:0"}))

	if !strings.Contains(after, "issues[25]{") {
		t.Fatalf("issue list did not render as a TOON table:\n%s", after[:min(len(after), 400)])
	}
	ratio := float64(len(after)) / float64(len(before))
	if ratio > 0.55 {
		t.Errorf("compact list is %.0f%% of the nested shape (%d vs %d chars), want under 55%%",
			ratio*100, len(after), len(before))
	}
	t.Logf("nested %d chars -> compact %d chars (%.0f%%)", len(before), len(after), ratio*100)

	// project is identical across every row, so it belongs in _all.
	if !strings.Contains(after, "_all:") {
		t.Errorf("constant columns were not hoisted:\n%s", after)
	}
	if n := strings.Count(after, "kontainer-backend"); n != 1 {
		t.Errorf("project appears %d times, want hoisted once", n)
	}
	// The titles vary, so metaType/metaValue are redundant for every row and
	// the columns should be gone entirely.
	if strings.Contains(after, "metaValue") {
		t.Errorf("metaValue survived despite every title already stating it:\n%s", after)
	}
	// Every row must still be addressable.
	for i := range 25 {
		if !strings.Contains(after, fmt.Sprintf("60741%d", i)) {
			t.Errorf("issue id 60741%d missing — rows must stay cross-referenceable", i)
		}
	}
}

func rawFrame(i int, inApp bool) map[string]any {
	return map[string]any{
		"function": "App\\Http\\Controllers\\AssetController::show",
		"filename": "app/Http/Controllers/AssetController.php",
		"absPath":  "/var/www/kontainer/app/Http/Controllers/AssetController.php",
		"lineNo":   float64(140 + i),
		"in_app":   inApp,
		"context": []any{
			[]any{float64(139 + i), "    $asset = Asset::find($id);"},
			[]any{float64(140 + i), "    return $asset->id;"},
		},
	}
}

func TestFrameRowDropsRedundantAbsPath(t *testing.T) {
	row := frameRow(rawFrame(0, true))
	assertAllScalar(t, "frameRow", row)
	if _, ok := row["absPath"]; ok {
		t.Error("absPath ends with filename and should be dropped as redundant")
	}
	if row["inApp"] != true {
		t.Errorf("inApp = %v, want the in_app value normalized", row["inApp"])
	}

	// A genuinely different absPath is kept — it is information, not a repeat.
	odd := rawFrame(0, true)
	odd["absPath"] = "/opt/vendor/other/Thing.php"
	if _, ok := frameRow(odd)["absPath"]; !ok {
		t.Error("a non-redundant absPath must survive")
	}
}

func TestExceptionValuesCapsFramesAndKeepsOneSourceWindow(t *testing.T) {
	frames := make([]any, 0, 40)
	for i := range 40 {
		frames = append(frames, rawFrame(i, i > 36)) // last few are in-app
	}
	values := []any{map[string]any{
		"type":       "TypeError",
		"value":      "Cannot read property 'id' of undefined",
		"stacktrace": map[string]any{"frames": frames},
	}}

	out := exceptionValues(values, 12)
	if len(out) != 1 {
		t.Fatalf("value count = %d, want 1", len(out))
	}
	exc := out[0].(map[string]any)
	got := exc["frames"].([]any)
	if len(got) != 12 {
		t.Errorf("frames = %d, want the cap of 12", len(got))
	}
	if exc["frames_omitted"] != 28 {
		t.Errorf("frames_omitted = %v, want 28", exc["frames_omitted"])
	}
	for i, f := range got {
		assertAllScalar(t, fmt.Sprintf("frame %d", i), f.(map[string]any))
	}

	// Exactly one source window, for the deepest in-app frame — not context
	// lines on all 40, which is what makes a trace dominate a payload.
	src := exc["source"].(map[string]any)
	if src["lineNo"] != float64(179) {
		t.Errorf("source lineNo = %v, want the deepest in-app frame (179)", src["lineNo"])
	}
	if lines := src["lines"].([]string); len(lines) != 2 || !strings.HasPrefix(lines[0], "178| ") {
		t.Errorf("source lines = %#v, want \"<lineNo>| <text>\" entries", src["lines"])
	}
	rendered := toonOf(t, exc)
	if strings.Count(rendered, "$asset = Asset::find") > 1 {
		t.Errorf("source context appears on more than one frame:\n%s", rendered)
	}
}

func TestEventEntryProjections(t *testing.T) {
	msg := eventEntry(map[string]any{
		"type": "message",
		"data": map[string]any{"formatted": "Queue drained", "message": "raw"},
	}, maxEventFrames)
	if msg["message"] != "Queue drained" {
		t.Errorf("message = %v, want the formatted form", msg["message"])
	}

	crumbs := make([]any, 0, 30)
	for i := range 30 {
		crumbs = append(crumbs, map[string]any{
			"timestamp": "2026-09-10T18:02:11.552091Z",
			"category":  "query",
			"level":     "info",
			"message":   fmt.Sprintf("select %d", i),
			"data":      map[string]any{"noise": strings.Repeat("x", 200)},
		})
	}
	bc := eventEntry(map[string]any{"type": "breadcrumbs", "data": map[string]any{"values": crumbs}}, maxEventFrames)
	kept := bc["values"].([]any)
	if len(kept) != maxBreadcrumbs {
		t.Errorf("breadcrumbs = %d, want the cap of %d", len(kept), maxBreadcrumbs)
	}
	if bc["omitted"] != 30-maxBreadcrumbs {
		t.Errorf("omitted = %v, want %d", bc["omitted"], 30-maxBreadcrumbs)
	}
	for i, c := range kept {
		assertAllScalar(t, fmt.Sprintf("crumb %d", i), c.(map[string]any))
	}
	if strings.Contains(toonOf(t, bc), "noise") {
		t.Error("the crumb data bag rode along; it belongs behind the _full endpoint")
	}

	// A request entry keeps the addressing and leaves headers/cookies out —
	// they are bulky and the likeliest place for a token to sit.
	req := eventEntry(map[string]any{"type": "request", "data": map[string]any{
		"method": "POST", "url": "https://app/api/assets", "query": "id=7",
		"headers": []any{[]any{"Authorization", "Bearer secret-token"}},
		"cookies": []any{[]any{"session", "abc"}},
	}}, maxEventFrames)
	if req["method"] != "POST" || req["url"] != "https://app/api/assets" {
		t.Errorf("request entry lost its addressing: %#v", req)
	}
	if rendered := toonOf(t, req); strings.Contains(rendered, "secret-token") || strings.Contains(rendered, "session") {
		t.Errorf("request headers/cookies must not be returned:\n%s", rendered)
	}

	// An entry with no projection collapses to a marker; available_types and
	// the _full endpoint are what get it back.
	other := eventEntry(map[string]any{"type": "debugmeta", "data": map[string]any{"images": []any{1, 2, 3}}}, maxEventFrames)
	if other["_omitted"] != true {
		t.Errorf("unprojected entry = %#v, want an _omitted marker", other)
	}
}
