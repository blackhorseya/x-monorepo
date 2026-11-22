package acceptance

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Eventually returns as soon as the condition holds, and says how long it waited.
func TestEventually_Succeeds(t *testing.T) {
	var calls atomic.Int32
	waited, err := Eventually(context.Background(), "the third call", 5*time.Second, func(context.Context) (string, bool, error) {
		n := calls.Add(1)
		return "calls=" + string(rune('0'+n)), n >= 3, nil
	})
	if err != nil {
		t.Fatalf("Eventually = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("check ran %d times, want 3", calls.Load())
	}
	if waited <= 0 || waited > 4*time.Second {
		t.Fatalf("waited = %s", waited)
	}
}

// A condition that never holds fails at the limit with what was asked for and the last value seen.
func TestEventually_TimeoutReportsLastObservation(t *testing.T) {
	var n atomic.Int32
	waited, err := Eventually(context.Background(), "inbox row to appear", 300*time.Millisecond, func(context.Context) (string, bool, error) {
		return "inbox rows=" + string(rune('0'+n.Add(1)%10)), false, nil
	})
	if err == nil {
		t.Fatal("Eventually succeeded on a condition that never holds")
	}
	for _, want := range []string{"inbox row to appear", "last observed: inbox rows=", "300ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "test deadline") {
		t.Errorf("the limit was the bound, but the error blames the test deadline: %v", err)
	}
	if waited < 300*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited = %s, want about the 300ms limit", waited)
	}
}

// The context's deadline bounds the wait when it is nearer than the limit, and the error says so.
func TestEventually_BoundedByContextDeadline(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	waited, err := Eventually(c, "never", time.Minute, func(context.Context) (string, bool, error) { return "x=1", false, nil })
	if err == nil {
		t.Fatal("Eventually succeeded")
	}
	if !strings.Contains(err.Error(), "test deadline") || !strings.Contains(err.Error(), "last observed: x=1") {
		t.Fatalf("error = %v", err)
	}
	if waited > 5*time.Second {
		t.Fatalf("waited %s, the context allowed 200ms", waited)
	}
}

// An error from check ends the wait at once, with what was asked for and the cause.
func TestEventually_CheckError(t *testing.T) {
	boom := errors.New("query failed")
	var calls atomic.Int32
	_, err := Eventually(context.Background(), "a row", time.Minute, func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "", false, boom
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "a row") {
		t.Fatalf("error = %v, want it to wrap boom and name what was awaited", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("check ran %d times after an error, want 1", calls.Load())
	}
}

// A cancelled context stops the wait without waiting for the limit.
func TestEventually_ContextCancelled(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := Eventually(c, "a row", time.Minute, func(context.Context) (string, bool, error) { return "rows=0", false, nil })
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s to notice the cancel", time.Since(start))
	}
}
