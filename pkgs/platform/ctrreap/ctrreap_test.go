//go:build unix

package ctrreap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// fakeAPI honours the label filter the way the runtime does, so a Reap that drops the filter lists the unmarked
// containers too and the test sees it.
type fakeAPI struct {
	mu        sync.Mutex
	all       []container.Summary
	removed   []string
	listErr   error
	removeErr map[string]error
}

func (f *fakeAPI) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	var want []string
	for l := range o.Filters["label"] {
		want = append(want, l)
	}
	var out []container.Summary
	for _, s := range f.all {
		ok := true
		for _, w := range want {
			k, v, has := strings.Cut(w, "=")
			if got, present := s.Labels[k]; !present || (has && got != v) {
				ok = false
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return client.ContainerListResult{Items: out}, nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.removeErr[id]; err != nil {
		return client.ContainerRemoveResult{}, err
	}
	f.removed = append(f.removed, id)
	return client.ContainerRemoveResult{}, nil
}

func labels(ns string, o Owner) map[string]string {
	return map[string]string{
		LabelNamespace: ns, LabelHost: o.Host,
		LabelPID: strconv.Itoa(o.PID), LabelStart: strconv.FormatInt(o.StartMS, 10),
	}
}

var self = Owner{Host: "h1", PID: 100, StartMS: 5000}

// probe: pid 100 is alive since 5000, pid 200 alive since 9000, pid 300 errors, everything else does not exist.
func probe(pid int) (int64, bool, error) {
	switch pid {
	case 100:
		return 5000, true, nil
	case 200:
		return 9000, true, nil
	case 300:
		return 0, false, errors.New("permission denied")
	}
	return 0, false, nil
}

func TestReapDecisionTable(t *testing.T) {
	api := &fakeAPI{all: []container.Summary{
		{ID: "dead", Labels: labels("ns", Owner{"h1", 999, 1})},
		{ID: "reused", Labels: labels("ns", Owner{"h1", 100, 4999})},
		{ID: "alive", Labels: labels("ns", Owner{"h1", 100, 5000})},
		{ID: "alive2", Labels: labels("ns", Owner{"h1", 200, 9000})},
		{ID: "elsewhere", Labels: labels("ns", Owner{"h2", 999, 1})},
		{ID: "probeerr", Labels: labels("ns", Owner{"h1", 300, 1})},
		{ID: "nolabels", Labels: map[string]string{LabelNamespace: "ns"}},
		{ID: "badpid", Labels: map[string]string{LabelNamespace: "ns", LabelHost: "h1", LabelPID: "x", LabelStart: "1"}},
		{ID: "unmarked", Labels: labels("", Owner{"h1", 999, 1})}, // dead owner but no marker value of ours
		{ID: "othership", Labels: labels("other", Owner{"h1", 999, 1})},
	}}
	delete(api.all[8].Labels, LabelNamespace)

	rep, err := Reap(context.Background(), api, "ns", self, probe)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Outcome{
		"dead": Removed, "reused": Removed, "alive": KeptAlive, "alive2": KeptAlive,
		"elsewhere": KeptOtherHost, "probeerr": KeptProbe, "nolabels": KeptUnreadable, "badpid": KeptUnreadable,
	}
	got := map[string]Outcome{}
	for _, it := range rep.Items {
		got[it.ID] = it.Outcome
	}
	for id, o := range want {
		if got[id] != o {
			t.Errorf("%s: %q, want %q", id, got[id], o)
		}
	}
	if len(got) != len(want) {
		t.Errorf("looked at %v, want exactly %d containers (unmarked / other namespace must not be listed)", got, len(want))
	}
	if len(api.removed) != 2 || api.removed[0] != "dead" || api.removed[1] != "reused" {
		t.Errorf("removed %v, want [dead reused]", api.removed)
	}
}

func TestReapRemoveFailureContinues(t *testing.T) {
	api := &fakeAPI{
		all: []container.Summary{
			{ID: "a", Labels: labels("ns", Owner{"h1", 999, 1})},
			{ID: "b", Labels: labels("ns", Owner{"h1", 998, 1})},
		},
		removeErr: map[string]error{"a": errors.New("busy")},
	}
	rep, err := Reap(context.Background(), api, "ns", self, probe)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Items[0].Outcome != RemoveFailed || rep.Items[1].Outcome != Removed {
		t.Fatalf("outcomes %v", rep.Items)
	}
	if len(api.removed) != 1 || api.removed[0] != "b" {
		t.Fatalf("removed %v, want [b]: a failure on one container must not stop the rest", api.removed)
	}
}

func TestReapNotFoundIsIdempotent(t *testing.T) {
	api := &fakeAPI{
		all:       []container.Summary{{ID: "a", Labels: labels("ns", Owner{"h1", 999, 1})}},
		removeErr: map[string]error{"a": fmt.Errorf("remove: %w", cerrdefs.ErrNotFound)},
	}
	rep, err := Reap(context.Background(), api, "ns", self, probe)
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Items[0]; got.Outcome != Removed || !strings.Contains(got.Detail, "already gone") {
		t.Fatalf("a container another reaper removed first is not a failure: %+v", got)
	}
}

func TestReapRemovalInProgressIsNotAFailure(t *testing.T) {
	inProgress := fmt.Errorf("removal of container a is already in progress: %w", cerrdefs.ErrConflict)
	other := fmt.Errorf("cannot remove a running container: %w", cerrdefs.ErrConflict)
	api := &fakeAPI{
		all: []container.Summary{
			{ID: "a", Labels: labels("ns", Owner{"h1", 999, 1})},
			{ID: "b", Labels: labels("ns", Owner{"h1", 998, 1})},
		},
		removeErr: map[string]error{"a": inProgress, "b": other},
	}
	rep, err := Reap(context.Background(), api, "ns", self, probe)
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Items[0]; got.Outcome != Removed || !strings.Contains(got.Detail, "in progress") {
		t.Fatalf("a removal another reaper is already doing is not a failure: %+v", got)
	}
	if got := rep.Items[1]; got.Outcome != RemoveFailed {
		t.Fatalf("any other conflict is still a failure: %+v", got)
	}
}

func TestReapListFailure(t *testing.T) {
	api := &fakeAPI{listErr: errors.New("daemon down")}
	if _, err := Reap(context.Background(), api, "ns", self, probe); err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("err = %v, want it to carry the list failure", err)
	}
}

// ReapOnce on a failing runtime: a fixed-format warning, no panic, nothing removed. It runs once per process, so this
// test must be the only one to call it.
func TestReapOnceWarnsAndDoesNotPanic(t *testing.T) {
	reapMu.Lock()
	reapOnce, lastReport, reaped = sync.Once{}, Report{}, false // once per process: give this test (and -count=2) its own
	reapMu.Unlock()
	var buf bytes.Buffer
	ReapOnce(context.Background(), &fakeAPI{listErr: errors.New("daemon down")}, &buf)
	ReapOnce(context.Background(), &fakeAPI{listErr: errors.New("again")}, &buf) // second call: no effect
	want := "ctrreap: warning: reap skipped: list containers: daemon down\n"
	if buf.String() != want {
		t.Fatalf("warning = %q, want %q", buf.String(), want)
	}
	if _, ok := LastReport(); ok {
		t.Fatal("a failed reap must not be reported as done")
	}
}

func TestProcessStartSelfAndGone(t *testing.T) {
	start, running, err := ProcessStart(os.Getpid())
	if err != nil || !running || start <= 0 {
		t.Fatalf("self: %d %v %v", start, running, err)
	}
	if _, running, err := ProcessStart(0); err == nil || running {
		t.Fatalf("pid 0 must be an error, got running=%v err=%v", running, err)
	}
}

func TestLabelsCarryOwner(t *testing.T) {
	t.Setenv(EnvNamespace, "unit")
	l := Labels()
	o, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	if l[LabelNamespace] != "unit" || l[LabelHost] != o.Host || l[LabelPID] != strconv.Itoa(o.PID) || l[LabelStart] != strconv.FormatInt(o.StartMS, 10) {
		t.Fatalf("labels %v do not describe this process %+v", l, o)
	}
}

// handleSignal: the terminators run, the exit code is 128+signal and never 0, and a terminator that ignores its
// context cannot hold the process past the grace.
func TestHandleSignalExitCodeAndTerminators(t *testing.T) {
	for _, tc := range []struct {
		sig  os.Signal
		code int
	}{{syscall.SIGTERM, 143}, {syscall.SIGINT, 130}} {
		resetTerminators(t)
		ran := make(chan struct{})
		sigMu.Lock()
		terminators = append(terminators, func(context.Context) { close(ran) })
		sigMu.Unlock()
		ch := make(chan os.Signal, 1)
		ch <- tc.sig
		exited := make(chan int, 1)
		handleSignal(ch, time.Second, func(c int) { exited <- c })
		select {
		case <-ran:
		default:
			t.Fatalf("%v: the terminator did not run before exit", tc.sig)
		}
		if c := <-exited; c != tc.code {
			t.Errorf("%v: exit code %d, want %d", tc.sig, c, tc.code)
		}
	}
}

func TestHandleSignalDeadlineBoundsABlockingTerminator(t *testing.T) {
	resetTerminators(t)
	block := make(chan struct{})
	defer close(block)
	sigMu.Lock()
	terminators = append(terminators, func(context.Context) { <-block })
	sigMu.Unlock()
	ch := make(chan os.Signal, 1)
	ch <- syscall.SIGTERM
	exited := make(chan int, 1)
	start := time.Now()
	handleSignal(ch, 200*time.Millisecond, func(c int) { exited <- c })
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("exit took %v: a blocking terminator held the process past its grace", d)
	}
	if c := <-exited; c != 143 {
		t.Fatalf("exit code %d, want 143", c)
	}
}

// A second signal while the terminators are still running ends the process at once, and the handler does not
// signal.Reset: a Notify channel somebody else owns keeps receiving.
func TestHandleSignalSecondSignalExitsAtOnce(t *testing.T) {
	resetTerminators(t)
	block := make(chan struct{})
	defer close(block)
	sigMu.Lock()
	terminators = append(terminators, func(context.Context) { <-block })
	sigMu.Unlock()
	other := make(chan os.Signal, 1)
	signal.Notify(other, syscall.SIGUSR2)
	defer signal.Stop(other)

	ch := make(chan os.Signal, 2)
	ch <- syscall.SIGTERM
	exits := make(chan int, 2)
	go handleSignal(ch, time.Hour, func(c int) { exits <- c })
	time.Sleep(100 * time.Millisecond) // the first signal is being handled, its terminator is blocked
	ch <- syscall.SIGINT
	select {
	case c := <-exits:
		if c != 143 {
			t.Fatalf("exit code %d, want 143", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second signal did not end the process while a terminator was blocked")
	}
}

func TestOnSignalDoesNotResetOthersChannels(t *testing.T) {
	other := make(chan os.Signal, 1)
	signal.Notify(other, syscall.SIGUSR1)
	defer signal.Stop(other)
	OnSignal(func(context.Context) {})
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("somebody else's Notify channel stopped receiving")
	}
}

// resetTerminators gives a test its own terminator list and restores the process's.
func resetTerminators(t *testing.T) {
	t.Helper()
	sigMu.Lock()
	saved := terminators
	terminators = nil
	sigMu.Unlock()
	t.Cleanup(func() {
		sigMu.Lock()
		terminators = saved
		sigMu.Unlock()
	})
}
