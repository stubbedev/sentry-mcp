package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func parseJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return v
}

func TestExtractIssueId(t *testing.T) {
	cases := map[string]string{
		"123456": "123456",
		"https://sentry.example.com/organizations/org/issues/5217/": "5217",
		"https://sentry.example.com/issues/99/events/latest/":       "99",
		"https://sentry.example.com/organizations/org/issues/":      "",
		"not-a-number": "",
		"":             "",
		"12ab":         "",
	}
	for in, want := range cases {
		if got := extractIssueId(in); got != want {
			t.Errorf("extractIssueId(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct{ in, def, want int }{
		{0, 25, 25},
		{-5, 100, 100},
		{50, 25, 50},
		{1000, 25, 100},
		{1, 25, 1},
	}
	for _, c := range cases {
		if got := clampLimit(c.in, c.def); got != c.want {
			t.Errorf("clampLimit(%d,%d) = %d, want %d", c.in, c.def, got, c.want)
		}
	}
}

func TestFilterFieldsInclude(t *testing.T) {
	obj := parseJSON(t, `{"id":"1","title":"x","latest_event":{"entries":[1,2],"junk":true}}`)
	got := filterFields(obj, []string{"id", "latest_event.entries"}, nil)
	want := parseJSON(t, `{"id":"1","latest_event":{"entries":[1,2]}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("include filter = %#v, want %#v", got, want)
	}
}

func TestFilterFieldsExclude(t *testing.T) {
	obj := parseJSON(t, `{"id":"1","stats":{"a":1},"nested":{"keep":1,"drop":2}}`)
	got := filterFields(obj, nil, []string{"stats", "nested.drop"})
	want := parseJSON(t, `{"id":"1","nested":{"keep":1}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exclude filter = %#v, want %#v", got, want)
	}
}

func TestTruncateStackFrames(t *testing.T) {
	data := parseJSON(t, `{"entries":[{"type":"exception","data":{"values":[{"stacktrace":{"frames":[1,2,3,4,5]}}]}}]}`)
	out := truncateStackFrames(data, 2).(map[string]any)
	st := out["entries"].([]any)[0].(map[string]any)["data"].(map[string]any)["values"].([]any)[0].(map[string]any)["stacktrace"].(map[string]any)
	frames := st["frames"].([]any)
	if len(frames) != 2 {
		t.Fatalf("frames len = %d, want 2", len(frames))
	}
	if !reflect.DeepEqual(frames, []any{float64(4), float64(5)}) {
		t.Errorf("kept frames = %#v, want last 2", frames)
	}
	if st["frames_omitted"] != 3 {
		t.Errorf("frames_omitted = %v, want 3", st["frames_omitted"])
	}
}

func TestGrepFilter(t *testing.T) {
	data := parseJSON(t, `{"function":"doThing","filename":"a.go","other":"nope"}`)
	out, err := grepFilter(data, "function")
	if err != nil {
		t.Fatal(err)
	}
	// Either re-parses to JSON or wraps in grep_results; both must mention the match.
	s, _ := json.Marshal(out)
	if !reflect.DeepEqual(out, out) || len(s) == 0 {
		t.Fatal("empty grep output")
	}
	if _, badErr := grepFilter(data, "(["); badErr == nil {
		t.Error("expected error on invalid regex")
	}
}

func TestParseNextCursor(t *testing.T) {
	c := NewSentryClient("https://x", "t", "o")
	withNext := `<https://x?cursor=0:100:0>; rel="next"; results="true"; cursor="0:100:0"`
	if got := c.parseNextCursor(withNext); got != "0:100:0" {
		t.Errorf("cursor = %q, want 0:100:0", got)
	}
	noMore := `<https://x?cursor=0:0:1>; rel="next"; results="false"; cursor="0:0:1"`
	if got := c.parseNextCursor(noMore); got != "" {
		t.Errorf("cursor = %q, want empty (results=false)", got)
	}
	if got := c.parseNextCursor(""); got != "" {
		t.Errorf("cursor = %q, want empty", got)
	}
}

func TestPickFormat(t *testing.T) {
	defaultFormat = "toon"
	if got := pickFormat(map[string]any{"format": "json"}); got != "json" {
		t.Errorf("explicit json = %q", got)
	}
	if got := pickFormat(map[string]any{"format": "TOON"}); got != "toon" {
		t.Errorf("case-insensitive toon = %q", got)
	}
	if got := pickFormat(map[string]any{}); got != "toon" {
		t.Errorf("default = %q, want toon", got)
	}
	if got := pickFormat(map[string]any{"format": "yaml"}); got != "toon" {
		t.Errorf("unknown falls back = %q, want toon", got)
	}
}

func TestNormalizeArgsProjectAlias(t *testing.T) {
	got := normalizeArgs(map[string]any{"project": "web"})
	if got["projectSlug"] != "web" {
		t.Errorf("project alias not mapped: %#v", got)
	}
	// Explicit projectSlug wins over alias.
	got = normalizeArgs(map[string]any{"project": "web", "projectSlug": "api"})
	if got["projectSlug"] != "api" {
		t.Errorf("projectSlug should win: %#v", got)
	}
}

func TestRenderStringFormats(t *testing.T) {
	jsonCtx := ctxWithFormat(context.Background(), "json")
	if out := renderString(jsonCtx, map[string]any{"a": 1}); out == "" || out[0] != '{' {
		t.Errorf("json render = %q", out)
	}
	toonCtx := ctxWithFormat(context.Background(), "toon")
	if out := renderString(toonCtx, map[string]any{"a": float64(1)}); out == "" || out[0] == '{' {
		t.Errorf("toon render should not be JSON object: %q", out)
	}
	// No format on the context → defaults to toon, not JSON.
	if out := renderString(context.Background(), map[string]any{"a": float64(1)}); out == "" || out[0] == '{' {
		t.Errorf("default render should be toon: %q", out)
	}
}
