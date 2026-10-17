package usecasex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

func TestMetrics_KindLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec, err := usecasex.NewRecorder(reg)
	if err != nil {
		t.Fatal(err)
	}

	var fail error
	uc := usecasex.Chain[int, int](
		usecasex.Func[int, int](func(_ context.Context, in int) (int, error) { return in, fail }),
		usecasex.Metrics[int, int](rec, "test.echo"),
	)

	c := context.Background()
	_, _ = uc.Execute(c, 1) // ok
	fail = kernel.NewError(kernel.KindNotFound, "TEST_NOT_FOUND", "nope")
	_, _ = uc.Execute(c, 1) // not_found
	_, _ = uc.Execute(c, 1) // not_found
	fail = errors.New("boom")
	_, _ = uc.Execute(c, 1) // internal

	got := map[string]uint64{}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != "usecase_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["usecase"] != "test.echo" {
				t.Fatalf("usecase label = %q", labels["usecase"])
			}
			got[labels["kind"]] = m.GetHistogram().GetSampleCount()
		}
	}

	want := map[string]uint64{"ok": 1, "not_found": 2, "internal": 1}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("kind %q count = %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected kinds: %v", got)
	}
}

func TestNewRecorder_DuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := usecasex.NewRecorder(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := usecasex.NewRecorder(reg); err == nil {
		t.Fatal("second registration on the same registry should fail")
	}
}
