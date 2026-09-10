package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flaky serves the given statuses in order, one per attempt, and answers 200
// for every attempt past the end of the list. It counts attempts so a test can
// assert how many times one logical request was actually sent.
type flaky struct {
	statuses   []int
	retryAfter string
	attempts   atomic.Int32
	srv        *httptest.Server
}

func newFlaky(t *testing.T, retryAfter string, statuses ...int) (*SentryClient, *flaky) {
	t.Helper()
	f := &flaky{statuses: statuses, retryAfter: retryAfter}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(f.attempts.Add(1))
		if n <= len(f.statuses) {
			if f.retryAfter != "" {
				w.Header().Set("Retry-After", f.retryAfter)
			}
			w.WriteHeader(f.statuses[n-1])
			w.Write([]byte(`{"detail":"transient"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return NewSentryClient(f.srv.URL, "tok", "konform"), f
}

func TestRequestRetriesTransientFailures(t *testing.T) {
	cases := []struct {
		name         string
		method       string
		statuses     []int
		wantAttempts int32
		wantErr      bool
	}{
		// 429 says the request was rejected before it did anything, so it is
		// safe to repeat for any method — including POST.
		{"rate limit then success", "GET", []int{429}, 2, false},
		{"rate limited POST is still retried", "POST", []int{429}, 2, false},
		// A gateway error may already have been applied, so only methods that
		// are safe to repeat get another go.
		{"gateway error on a read", "GET", []int{502}, 2, false},
		{"gateway error on a PUT", "PUT", []int{503}, 2, false},
		{"gateway error on a POST is not retried", "POST", []int{502}, 1, true},
		// Nothing transient about a 4xx we caused.
		{"not found is final", "GET", []int{404}, 1, true},
		{"unauthorized is final", "GET", []int{401}, 1, true},
		// Persistent failure stops at the attempt ceiling.
		{"gives up after maxAttempts", "GET", []int{503, 503, 503, 503}, int32(maxAttempts), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFlaky(t, "", tc.statuses...)
			_, err := c.request(context.Background(), tc.method, "/issues/1/", nil, nil)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := f.attempts.Load(); got != tc.wantAttempts {
				t.Errorf("sent %d attempts, want %d", got, tc.wantAttempts)
			}
		})
	}
}

func TestRequestReplaysTheBody(t *testing.T) {
	// A retried write must send its body again: the first attempt consumes the
	// reader, so a single shared reader would send an empty second request.
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewSentryClient(srv.URL, "tok", "konform")
	if _, err := c.request(context.Background(), "PUT", "/issues/1/", nil, map[string]any{"status": "resolved"}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d attempts, want 2", len(bodies))
	}
	if bodies[0] != bodies[1] || bodies[1] == "" {
		t.Errorf("body not replayed: %q then %q", bodies[0], bodies[1])
	}
}

func TestRequestHonoursRetryAfter(t *testing.T) {
	c, f := newFlaky(t, "1", 429)
	start := time.Now()
	if _, err := c.request(context.Background(), "GET", "/issues/1/", nil, nil); err != nil {
		t.Fatalf("request: %v", err)
	}
	// The server asked for a second; the default backoff is 250ms, so obeying
	// the header is observable.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("waited %v, want at least the 1s Retry-After", elapsed)
	}
	if got := f.attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestRequestDoesNotRetryPastTheDeadline(t *testing.T) {
	// A Retry-After longer than the remaining tool-call budget must be
	// declined, not slept through only to fail at the end of it.
	c, f := newFlaky(t, "30", 429)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.request(ctx, "GET", "/issues/1/", nil, nil); err == nil {
		t.Fatal("expected the rate-limit error to surface")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v — the wait should have been declined outright", elapsed)
	}
	if got := f.attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry within the deadline)", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Errorf("seconds form = %v, want 2s", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
	if got := parseRetryAfter("-5"); got != 0 {
		t.Errorf("negative = %v, want 0", got)
	}
	if got := parseRetryAfter("not a delay"); got != 0 {
		t.Errorf("garbage = %v, want 0", got)
	}
	// The HTTP-date form, which Sentry behind a proxy may send.
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 4*time.Second {
		t.Errorf("date form = %v, want ~3s", got)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("past date = %v, want 0", got)
	}
}

func TestWaitBeforeRetryDeclinesAbsurdWaits(t *testing.T) {
	if waitBeforeRetry(context.Background(), maxRetryWait+time.Second) {
		t.Error("a wait past maxRetryWait should be declined")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if waitBeforeRetry(cancelled, time.Millisecond) {
		t.Error("a cancelled context should not wait")
	}
	if !waitBeforeRetry(context.Background(), time.Millisecond) {
		t.Error("a short wait on a live context should complete")
	}
}

func TestBuildInstructionsDoesNotBlockOnASlowSentry(t *testing.T) {
	// The regression this guards: these calls used to run sequentially on
	// context.Background() with a 30s client timeout, inside main() before the
	// transport started — so an unresponsive instance left a GUI client looking
	// hung for up to a minute with no tools listed.
	// Hold each request until the client gives up on it, so the handler
	// unblocks on its own and srv.Close() cannot deadlock against it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	sentry = NewSentryClient(srv.URL, "tok", "konform")
	defer func() { sentry = nil }()

	old := instructionsTimeout
	instructionsTimeout = 150 * time.Millisecond
	defer func() { instructionsTimeout = old }()

	cfg := Config{Sentry: &SentryConfig{URL: srv.URL, Token: "tok", Org: "konform"}}
	done := make(chan string, 1)
	start := time.Now()
	go func() { done <- buildInstructions(cfg) }()

	select {
	case out := <-done:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("buildInstructions took %v despite a %v budget", elapsed, instructionsTimeout)
		}
		// It must still produce usable instructions, just without the parts
		// that needed the unreachable server.
		for _, want := range []string{"# sentry-mcp", "Configured instance", "Reading these responses"} {
			if !strings.Contains(out, want) {
				t.Errorf("degraded instructions missing %q", want)
			}
		}
		if strings.Contains(out, "## Projects") {
			t.Error("project list should be absent when discovery timed out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("buildInstructions never returned — startup would hang")
	}
}
