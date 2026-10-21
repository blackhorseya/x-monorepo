package usecasex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type fakeInbox struct {
	seen  map[string]bool
	calls int
	err   error
}

func (f *fakeInbox) Record(_ context.Context, id string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if f.seen[id] {
		return false, nil
	}
	f.seen[id] = true
	return true, nil
}

func TestIdempotent(t *testing.T) {
	var ran int
	next := usecasex.Func[string, string](func(_ context.Context, in string) (string, error) {
		ran++
		return "out:" + in, nil
	})
	inbox := &fakeInbox{seen: map[string]bool{}}
	uc := usecasex.Chain(next, usecasex.Idempotent[string, string](inbox))

	// rpc path: no message id → inbox untouched.
	if out, err := uc.Execute(context.Background(), "a"); err != nil || out != "out:a" || inbox.calls != 0 {
		t.Fatalf("no message id: out=%q err=%v inbox calls=%d", out, err, inbox.calls)
	}
	// first delivery runs; the second returns the duplicate error without running.
	c := contextx.WithMessageID(context.Background(), "m-1")
	if out, err := uc.Execute(c, "b"); err != nil || out != "out:b" {
		t.Fatalf("first: out=%q err=%v", out, err)
	}
	out, err := uc.Execute(c, "b")
	var de *kernel.Error
	if !errors.Is(err, usecasex.ErrDuplicateMessage) || !errors.As(err, &de) || de.Code() != "message.duplicate" || de.Kind() != kernel.KindConflict || out != "" {
		t.Fatalf("second: out=%q err=%v", out, err)
	}
	if ran != 2 {
		t.Fatalf("next ran %d times, want 2 (rpc + first delivery)", ran)
	}
	// another message id runs again.
	if _, err := uc.Execute(contextx.WithMessageID(context.Background(), "m-2"), "c"); err != nil || ran != 3 {
		t.Fatalf("m-2: err=%v ran=%d", err, ran)
	}

	// inbox failure is an infrastructure error (retried by the consumer), next does not run.
	inbox.err = errors.New("db down")
	if _, err := uc.Execute(contextx.WithMessageID(context.Background(), "m-3"), "d"); err == nil || errors.As(err, &de) || ran != 3 {
		t.Fatalf("inbox error: err=%v ran=%d", err, ran)
	}

	// nil inbox: pass-through even with a message id.
	plain := usecasex.Chain(next, usecasex.Idempotent[string, string](nil))
	if _, err := plain.Execute(c, "e"); err != nil || ran != 4 {
		t.Fatalf("nil inbox: err=%v ran=%d", err, ran)
	}
}
