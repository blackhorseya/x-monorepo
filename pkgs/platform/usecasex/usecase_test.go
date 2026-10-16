package usecasex_test

import (
	"context"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

func TestChain_Order(t *testing.T) {
	var trace []string
	mark := func(tag string) usecasex.Decorator[int, int] {
		return func(next usecasex.UseCase[int, int]) usecasex.UseCase[int, int] {
			return usecasex.Func[int, int](func(c context.Context, in int) (int, error) {
				trace = append(trace, tag+">")
				out, err := next.Execute(c, in)
				trace = append(trace, "<"+tag)
				return out, err
			})
		}
	}
	base := usecasex.Func[int, int](func(_ context.Context, in int) (int, error) { return in * 2, nil })

	uc := usecasex.Chain[int, int](base, mark("a"), mark("b"), usecasex.Logging[int, int]("test"))
	out, err := uc.Execute(context.Background(), 21)
	if err != nil || out != 42 {
		t.Fatalf("out = %d, err = %v", out, err)
	}
	want := []string{"a>", "b>", "<b", "<a"}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
}
