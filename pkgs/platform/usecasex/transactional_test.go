package usecasex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type txKey struct{}

// fakeTx marks the context it hands to fn and can fail the "commit".
type fakeTx struct {
	commitErr error
	calls     int
}

func (f *fakeTx) Do(c context.Context, fn func(c context.Context) error) error {
	f.calls++
	if err := fn(context.WithValue(c, txKey{}, true)); err != nil {
		return err
	}

	return f.commitErr
}

func TestTransactional(t *testing.T) {
	errUC := errors.New("use case failed")
	errCommit := errors.New("commit failed")

	for _, tc := range []struct {
		name      string
		ucErr     error
		commitErr error
		wantOut   *int
		wantErr   error
	}{
		{name: "ok", wantOut: ptr(2)},
		{name: "use case error rolls back", ucErr: errUC, wantErr: errUC},
		{name: "commit error discards output", commitErr: errCommit, wantErr: errCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &fakeTx{commitErr: tc.commitErr}
			uc := usecasex.Chain[int, *int](
				usecasex.Func[int, *int](func(c context.Context, in int) (*int, error) {
					if c.Value(txKey{}) != true {
						t.Fatal("use case ran outside the transaction context")
					}
					out := in * 2
					return &out, tc.ucErr
				}),
				usecasex.Transactional[int, *int](tx),
			)

			out, err := uc.Execute(context.Background(), 1)
			if tx.calls != 1 {
				t.Fatalf("Do calls = %d, want 1", tx.calls)
			}
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			switch {
			case tc.wantOut == nil && out != nil:
				t.Fatalf("out = %v, want nil on error", *out)
			case tc.wantOut != nil && (out == nil || *out != *tc.wantOut):
				t.Fatalf("out = %v, want %d", out, *tc.wantOut)
			}
		})
	}
}

func ptr(n int) *int { return &n }
