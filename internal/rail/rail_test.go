package rail

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hangServer accepts the connection but never responds, simulating a public
// Overpass mirror that hangs — the exact failure mode that used to eat the
// whole budget. The handler blocks until either the client cancels (what the
// per-try deadline triggers) or the test tears down. The explicit `done`
// channel is closed before s.Close() (cleanups run LIFO) so teardown can't
// hang waiting on a handler that the server didn't observe as disconnected.
func hangServer(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(s.Close)
	t.Cleanup(func() { close(done) })
	return s
}

// okServer returns a valid (empty) Overpass response immediately.
func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"elements":[]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestQueryBudget(t *testing.T) {
	// A hung mirror must be abandoned at ~perTry and the next mirror tried, so
	// the request succeeds well inside the total budget instead of running into
	// the platform's hard kill.
	t.Run("falls through hung mirror to a healthy one", func(t *testing.T) {
		hang, ok := hangServer(t), okServer(t)
		c := &Client{
			HTTP:    &http.Client{},
			Mirrors: []string{hang.URL, ok.URL},
			perTry:  150 * time.Millisecond,
			budget:  2 * time.Second,
		}
		start := time.Now()
		resp, err := c.query(context.Background(), "")
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("expected success after fallback, got %v", err)
		}
		if resp == nil {
			t.Fatal("expected non-nil response from healthy mirror")
		}
		if elapsed < 50*time.Millisecond {
			t.Fatalf("returned too fast (%v) — did it actually wait out the hung mirror?", elapsed)
		}
		if elapsed > time.Second {
			t.Fatalf("took %v — per-try deadline did not bail on the hung mirror", elapsed)
		}
	})

	// With every mirror hung, the total budget caps wall-clock. Without it,
	// three full 200ms tries would run ~600ms; the 300ms budget clamps the 2nd
	// try to the time remaining and skips the 3rd, so the loop returns by
	// ~300ms. Regression guard for the 504 — an unbounded doomed request ran
	// long enough for the platform to hard-kill the function.
	t.Run("total budget caps a doomed run", func(t *testing.T) {
		h1, h2, h3 := hangServer(t), hangServer(t), hangServer(t)
		c := &Client{
			HTTP:    &http.Client{},
			Mirrors: []string{h1.URL, h2.URL, h3.URL},
			perTry:  200 * time.Millisecond,
			budget:  300 * time.Millisecond,
		}
		start := time.Now()
		_, err := c.query(context.Background(), "")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected an error when all mirrors hang")
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("ran %v — budget did not cap the doomed run (naive would be ~600ms)", elapsed)
		}
	})

	// Caller cancellation (e.g. the function deadline firing) must return the
	// context error promptly rather than working through the remaining budget.
	t.Run("caller cancellation returns promptly", func(t *testing.T) {
		hang := hangServer(t)
		c := &Client{
			HTTP:    &http.Client{},
			Mirrors: []string{hang.URL},
			perTry:  10 * time.Second,
			budget:  20 * time.Second,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.query(ctx, "")
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context deadline error, got %v", err)
		}
		if elapsed > time.Second {
			t.Fatalf("took %v — cancellation was not honored promptly", elapsed)
		}
	})
}
