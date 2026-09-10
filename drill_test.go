package main

import (
	"strings"
	"testing"
)

func TestWalkPath(t *testing.T) {
	data := parseJSON(t, `{"entries":[{"type":"exception","data":{"values":[{"stacktrace":{"frames":[{"function":"boom"}]}}]}}]}`)

	got, err := walkPath(data, "entries.0.data.values.0.stacktrace.frames.0.function")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if got != "boom" {
		t.Errorf("walked to %v, want \"boom\"", got)
	}

	// An empty path is the whole document, which is what an outline call wants.
	if whole, err := walkPath(data, ""); err != nil || whole == nil {
		t.Errorf("empty path should return the root: %v, %v", whole, err)
	}
}

func TestWalkPathErrorsAreActionable(t *testing.T) {
	data := parseJSON(t, `{"entries":[{"type":"exception"}],"eventID":"abc"}`)

	// A missing key must name what was there instead, so the next attempt is
	// informed rather than another guess.
	_, err := walkPath(data, "entrys")
	if err == nil {
		t.Fatal("expected an error for a misspelled key")
	}
	for _, want := range []string{"entries", "eventID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list the available key %q", err, want)
		}
	}

	if _, err := walkPath(data, "entries.type"); err == nil || !strings.Contains(err.Error(), "array") {
		t.Errorf("indexing an array with a key should say so, got %v", err)
	}
	if _, err := walkPath(data, "entries.9"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("out-of-range index should say so, got %v", err)
	}
	if _, err := walkPath(data, "eventID.nope"); err == nil || !strings.Contains(err.Error(), "scalar") {
		t.Errorf("descending into a scalar should say so, got %v", err)
	}
}

func TestOutlineSketchesShapeNotContent(t *testing.T) {
	data := parseJSON(t, `{"eventID":"abc123","entries":[{"type":"exception","big":"aaaaaaaaaa"},{"type":"message"}],"ok":true,"n":3,"nil":null}`)
	sketch := outline(data, 0).(map[string]any)

	if sketch["eventID"] != "string(6)" {
		t.Errorf("string leaf = %v, want its length", sketch["eventID"])
	}
	if sketch["ok"] != "bool" || sketch["n"] != "number" || sketch["nil"] != "null" {
		t.Errorf("leaf types = %#v", sketch)
	}
	// An array is described by its length plus the shape of its first element,
	// which for Sentry's uniform arrays describes all of them.
	entries := sketch["entries"].(map[string]any)
	elem, ok := entries["[2]"]
	if !ok {
		t.Fatalf("array not described by length: %#v", entries)
	}
	if _, ok := elem.(map[string]any)["type"]; !ok {
		t.Errorf("array element shape missing its keys: %#v", elem)
	}
	// The point of a sketch is that content does not travel with it.
	if strings.Contains(toonOf(t, sketch), "aaaaaaaaaa") {
		t.Error("outline leaked the payload's contents")
	}
}

func TestOutlineDepthCap(t *testing.T) {
	deep := parseJSON(t, `{"a":{"b":{"c":{"d":{"e":{"f":1}}}}}}`)
	rendered := toonOf(t, outline(deep, 0))
	if !strings.Contains(rendered, "keys}") {
		t.Errorf("deep nesting should elide with a key count, got:\n%s", rendered)
	}
}

func TestPageText(t *testing.T) {
	s := strings.Repeat("ab", 50) // 100 chars

	if got := pageText(s, 0, 0).Content[0].Text; got != s {
		t.Error("no window means the whole payload")
	}

	first := pageText(s, 40, 0).Content[0].Text
	if !strings.HasPrefix(first, s[:40]) {
		t.Errorf("first page = %q", first)
	}
	if !strings.Contains(first, "charOffset=40") {
		t.Errorf("first page should say how to resume: %q", first)
	}

	last := pageText(s, 40, 80).Content[0].Text
	if last != s[80:] {
		t.Errorf("final page = %q, want the tail with no resume hint", last)
	}

	// Slicing is on runes: a multibyte payload must never be split
	// mid-character, which would emit invalid UTF-8.
	multi := strings.Repeat("é", 10)
	chunk := pageText(multi, 4, 0).Content[0].Text
	if !strings.HasPrefix(chunk, strings.Repeat("é", 4)) {
		t.Errorf("rune slicing broke a character: %q", chunk)
	}

	// An offset past the end is empty, not a panic.
	if got := pageText(s, 10, 500).Content[0].Text; got != "" {
		t.Errorf("offset past the end = %q, want empty", got)
	}
}
