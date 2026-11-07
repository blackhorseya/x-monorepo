//go:build unix

package reaptest

import (
	"syscall"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/ctrreap"
)

// Decisions is the "what may be removed, and nothing else" half of the guarantee, against the real runtime. Seven
// synthetic containers stand in for what a shared machine really has lying around, a fixture (the calling package's
// own test binary, real TestMain, real start) then starts and reaps, and each container must end up as its row says:
//
//	owner gone              marker + a dead owner on this host            removed
//	pid reused              marker + this very (live) process's pid, but another start time     removed
//	owner alive             marker + this live process                    KEPT
//	owner on another host   marker + a dead owner on some other host      KEPT
//	owner labels missing    marker only                                   KEPT
//	no marker label         a dead owner's labels but NO marker           KEPT, never even listed
//	another namespace       a dead owner, the marker of ANOTHER namespace KEPT, never even listed
//
// The first row is the positive control: it proves the reap really ran in this fixture and CAN remove, so a "kept"
// below means the reaper looked and refused, not that it did not run. Every container is checked to exist before the
// fixture starts. The fixture reports what it saw (ctrreap.LastReport), so a "kept" is checked at the reaper's
// decision, not only at the container still being there.
func Decisions(t *testing.T, image string) {
	t.Helper()
	cs := On(t, NeedRuntime(t))
	ns := Namespace(t)
	dead, self := DeadOwner(t), SelfOwner(t)
	reused := self
	reused.StartMS += 123456
	elsewhere := dead
	elsewhere.Host = "elsewhere-" + ns

	type row struct {
		name   string
		labels map[string]string
		listed bool            // the reaper's own filter must list it
		want   ctrreap.Outcome // when listed
		gone   bool            // must be gone afterwards
		id     string
	}
	rows := []*row{
		{name: "owner gone", labels: MarkerLabels(ns, dead), listed: true, want: ctrreap.Removed, gone: true},
		{name: "pid reused", labels: MarkerLabels(ns, reused), listed: true, want: ctrreap.Removed, gone: true},
		{name: "owner alive", labels: MarkerLabels(ns, self), listed: true, want: ctrreap.KeptAlive},
		{name: "owner on another host", labels: MarkerLabels(ns, elsewhere), listed: true, want: ctrreap.KeptOtherHost},
		{name: "owner labels missing", labels: map[string]string{ctrreap.LabelNamespace: ns}, listed: true, want: ctrreap.KeptUnreadable},
		{name: "no marker label", labels: OwnerLabels(dead)},
		{name: "another namespace", labels: MarkerLabels(ns+"-other", dead)},
	}
	for _, r := range rows {
		r.id = cs.Create(image, r.labels)
		if !cs.Exists(r.id) {
			t.Fatalf("reaptest: SETUP FAILURE, not a result: the synthetic container of %q is not there before the fixture starts", r.name)
		}
	}

	f := Spawn(t, Opts{Mode: "once", Namespace: ns})
	f.WaitReady(readyBudget)
	rep := f.Report()
	if code := f.Wait(60 * time.Second); code != 0 {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: the fixture exited %d:\n%s", code, f.Output())
	}
	if rep.Namespace != ns {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: the fixture reaped namespace %q, want %q", rep.Namespace, ns)
	}
	items := map[string]ctrreap.Item{}
	for _, it := range rep.Items {
		items[it.ID] = it
	}
	if got := items[rows[0].id]; got.Outcome != ctrreap.Removed {
		t.Fatalf("reaptest: SETUP FAILURE, not a result: the positive control (%q) was not removed by the fixture's reap: %+v\n%s", rows[0].name, got, f.Output())
	}

	for _, r := range rows {
		it, listed := items[r.id]
		if listed != r.listed {
			t.Errorf("%s: listed by the reaper = %v, want %v", r.name, listed, r.listed)
		} else if listed && it.Outcome != r.want {
			t.Errorf("%s: the reaper decided %q, want %q", r.name, it.Outcome, r.want)
		}
		if cs.Exists(r.id) == r.gone {
			t.Errorf("%s: the container is %s, want it %s", r.name, map[bool]string{true: "there", false: "gone"}[!r.gone], map[bool]string{true: "gone", false: "there"}[r.gone])
		}
	}
}

// Parallel is the machine two agents share: three test processes of the same namespace start at the same moment, each
// reaps, while a real orphan is waiting to be reaped. Nobody may remove a container whose owner is alive (each other's,
// including the one a process is creating while another is listing), the orphan goes, and a reaper that loses the race for
// it (the container is already gone) is not a failure. Then each is told to stop with SIGTERM and takes its container with it.
func Parallel(t *testing.T, image string) {
	t.Helper()
	cs := On(t, NeedRuntime(t))
	ns := Namespace(t)
	orphan := cs.Create(image, MarkerLabels(ns, DeadOwner(t)))

	fixtures := make([]*Child, 3)
	for i := range fixtures {
		fixtures[i] = Spawn(t, Opts{Namespace: ns})
	}
	ids := make([]string, len(fixtures))
	for i, f := range fixtures {
		ids[i] = cs.IDByPort(PortOf(t, f.WaitReady(readyBudget)))
		if ids[i] == "" {
			t.Fatalf("reaptest: SETUP FAILURE, not a result: fixture %d has no container", i)
		}
	}
	live := map[string]bool{}
	for _, id := range ids {
		live[id] = true
	}

	removedOrphan := false
	for i, f := range fixtures {
		for _, it := range f.Report().Items {
			switch {
			case it.Outcome == ctrreap.RemoveFailed:
				t.Errorf("fixture %d: could not remove %.12s: %s", i, it.ID, it.Detail)
			case it.Outcome == ctrreap.Removed && live[it.ID]:
				t.Errorf("fixture %d REMOVED %.12s, a container whose owner was alive", i, it.ID)
			case it.Outcome == ctrreap.Removed && it.ID == orphan:
				removedOrphan = true
			}
		}
	}
	for i, id := range ids {
		if !cs.Exists(id) {
			t.Errorf("fixture %d: its container %.12s is gone while it is alive", i, id)
		}
	}
	if !removedOrphan {
		t.Errorf("no fixture reported removing the real orphan %.12s: the race was not exercised", orphan)
	}
	// a reaper that lost the race returns while the winner's remove is still running (Docker: "already in progress")
	if !cs.GoneWithin(orphan, 30*time.Second) {
		t.Errorf("the orphan %.12s is still there after three reaps", orphan)
	}

	for _, f := range fixtures {
		f.Signal(syscall.SIGTERM)
	}
	for i, f := range fixtures {
		if code := f.Wait(60 * time.Second); code == 0 {
			t.Errorf("fixture %d exited 0 after SIGTERM", i)
		}
		if !cs.GoneWithin(ids[i], 5*time.Second) {
			t.Errorf("fixture %d: its container %.12s is still there after SIGTERM", i, ids[i])
		}
	}
}
