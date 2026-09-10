package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ── Drilling into a full payload ─────────────────────────────────────────────
//
// The projections in compact.go drop detail from a response, so something has
// to be able to get it back. The handle for that is the upstream endpoint
// itself, carried on the compact result as `_full`: a compact response says
// where the complete payload lives, and sentry_raw_api fetches it with a `path`
// to walk into just the part that is wanted.
//
// Deliberately not a server-side handle table. A ref that resolves against
// in-process memory needs session affinity to work at all — it dies on
// restart, does not resolve on a second replica, and assumes a session the
// protocol is moving away from guaranteeing (see rootsRemovedFrom in roots.go
// for the same drift). An endpoint string has none of those problems: it is
// valid on any instance, at any later point in the conversation, and costs one
// line to state.

// maxOutlineDepth caps how deep a shape sketch descends before eliding.
const maxOutlineDepth = 4

// outline sketches a value's shape — keys, array lengths, leaf types — so a
// caller can see what an endpoint returns and choose a path without pulling the
// payload itself. Arrays are described by their length and the shape of their
// first element, which for Sentry's uniform arrays describes all of them.
func outline(v any, depth int) any {
	switch t := v.(type) {
	case map[string]any:
		if depth >= maxOutlineDepth {
			return fmt.Sprintf("{%d keys}", len(t))
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = outline(val, depth+1)
		}
		return out
	case []any:
		if len(t) == 0 {
			return "[0]"
		}
		if depth >= maxOutlineDepth {
			return fmt.Sprintf("[%d]", len(t))
		}
		return map[string]any{fmt.Sprintf("[%d]", len(t)): outline(t[0], depth+1)}
	case string:
		return fmt.Sprintf("string(%d)", len(t))
	case nil:
		return "null"
	case bool:
		return "bool"
	default:
		return "number"
	}
}

// walkPath resolves a dot-separated path into v, where a numeric segment
// indexes an array ("entries.0.data.values.0.stacktrace"). A miss reports what
// was actually available at the point of failure, so the next attempt is
// informed rather than guessed.
func walkPath(v any, path string) (any, error) {
	cur := v
	for _, seg := range splitNonEmpty(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, fmt.Errorf("no %q at this level; available keys: %s", seg, strings.Join(sortedKeys(node), ", "))
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil {
				return nil, fmt.Errorf("%q indexes an array of %d — use a number, not a key", seg, len(node))
			}
			if i < 0 || i >= len(node) {
				return nil, fmt.Errorf("index %d is out of range (the array has %d)", i, len(node))
			}
			cur = node[i]
		default:
			return nil, fmt.Errorf("cannot descend into %q: the value above it is a scalar", seg)
		}
	}
	return cur, nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pageText slices a rendered payload by character window, appending the offset
// to resume from. Slicing is on runes, not bytes, so multibyte UTF-8 (common in
// Sentry payloads) is never split mid-character.
func pageText(s string, maxChars, charOffset int) toolResult {
	if charOffset <= 0 && maxChars <= 0 {
		return textResult(s)
	}
	runes := []rune(s)
	offset := min(max(charOffset, 0), len(runes))
	limit := maxChars
	if limit <= 0 {
		limit = len(runes)
	}
	end := min(offset+limit, len(runes))
	chunk := string(runes[offset:end])
	if remaining := len(runes) - end; remaining > 0 {
		chunk += fmt.Sprintf("\n\n... (%d more chars, use charOffset=%d)", remaining, end)
	}
	return textResult(chunk)
}
