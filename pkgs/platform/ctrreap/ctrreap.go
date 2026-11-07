// Package ctrreap keeps the throwaway containers of pgtest and redistest from outliving the test binary that started
// them (#481).
//
// Why it exists: on Podman Ryuk (testcontainers' own reaper) is switched off, because Ryuk hardcodes a docker network
// named "bridge" that Podman does not have (~/.testcontainers.properties: ryuk.disabled=true). pgtest.Main and
// redistest.Main terminate their container only after m.Run returns, so a test binary that ends any other way left its
// container behind for good. CI runs on Docker where Ryuk works, and is not changed by this: the reaper below only ever
// touches containers that carry this repo's label, and Ryuk's own containers do not.
//
// Two layers, and what each does not cover:
//
//	how the test binary ended                           what removes the container
//	normal exit                                         Terminate, as before
//	SIGINT / SIGTERM, test binary ending through Main   layer 1, OnSignal: terminated at once, exit 128+signal
//	SIGINT / SIGTERM, a package with its own TestMain   layer 2 only: such a package's tests send SIGTERM to their own
//	                                                    process (graceful-shutdown tests), which a handler would turn
//	                                                    into a container kill and exit 143 mid-test. apps/catalog-rpc,
//	                                                    apps/payment-rpc, apps/payment-worker (pgtest.Terminate by hand)
//	go test -timeout, a panic, SIGKILL                  layer 2, ReapBeforeStart: the next start in ANY process removes it
//	a signal while the container is still starting      layer 2 (layer 1 waits for the start up to its grace, then the
//	                                                    process leaves; the container the start creates later is an orphan)
//	no test is ever run on this machine again           nothing: it stays until the next run (a known limit)
//
// What may be removed, and nothing else: a container with this repo's marker label (Namespace) whose owner is gone.
// The owner is the test process, recorded on the container as host + pid + the process's start time. It is gone when
// that pid does not exist on this host, or exists with another start time (the pid was reused). A container whose owner
// is alive (another agent's tests are running on the same machine), whose owner is on another host, or whose owner
// labels are missing or unreadable is KEPT: "cannot prove it is dead" is never a reason to remove.
//
// A container without the marker label is not even listed. Removing it takes the marker AND readable owner labels.
package ctrreap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/shirou/gopsutil/v4/process"
)

// Labels written on every container pgtest and redistest start. The marker is the only thing Reap filters on; the owner
// labels are what it decides on. They are separate keys so that "has the marker" and "has an owner" can each be absent.
const (
	LabelNamespace = "io.github.blackhorseya.x-monorepo.testctr"
	LabelHost      = LabelNamespace + ".owner.host"
	LabelPID       = LabelNamespace + ".owner.pid"
	LabelStart     = LabelNamespace + ".owner.start" // the owner process's start time, ms since the epoch

	// EnvNamespace confines a process to a namespace of its own: it labels its containers with it and reaps only that
	// namespace's. The default is shared by every test process of the repo. The harness that tests this package sets
	// it so a fixture neither reaps nor is reaped by another process's reaper (other agents run tests on the same machine).
	EnvNamespace     = "X_TESTCTR_NAMESPACE"
	DefaultNamespace = "shared"
)

// Namespace is this process's namespace.
func Namespace() string {
	if v := os.Getenv(EnvNamespace); v != "" {
		return v
	}
	return DefaultNamespace
}

// Owner is the process a container belongs to.
type Owner struct {
	Host    string
	PID     int
	StartMS int64
}

// Self is this process. The start time comes from the same probe Reap uses on a candidate, so both sides of the
// comparison are read the same way.
func Self() (Owner, error) {
	host, err := os.Hostname()
	if err != nil {
		return Owner{}, fmt.Errorf("hostname: %w", err)
	}
	start, running, err := ProcessStart(os.Getpid())
	if err != nil {
		return Owner{}, fmt.Errorf("start time of pid %d: %w", os.Getpid(), err)
	}
	if !running {
		return Owner{}, fmt.Errorf("start time of pid %d: process not running", os.Getpid())
	}
	return Owner{Host: host, PID: os.Getpid(), StartMS: start}, nil
}

// Labels is what this process puts on a container it starts. When the owner cannot be determined only the marker is
// written: such a container is never reaped (the owner labels are required), which leaks but cannot remove a live one.
func Labels() map[string]string {
	l := map[string]string{LabelNamespace: Namespace()}
	if o, err := Self(); err == nil {
		l[LabelHost] = o.Host
		l[LabelPID] = strconv.Itoa(o.PID)
		l[LabelStart] = strconv.FormatInt(o.StartMS, 10)
	}
	return l
}

// ProcessStart is the start time of pid in ms since the epoch, and whether it is running at all.
func ProcessStart(pid int) (startMS int64, running bool, err error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return 0, false, fmt.Errorf("pid %d out of range", pid)
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		if errors.Is(err, process.ErrorProcessNotRunning) {
			return 0, false, nil
		}
		return 0, false, err
	}
	ct, err := p.CreateTime()
	if err != nil {
		if exists, perr := process.PidExists(int32(pid)); perr == nil && !exists {
			return 0, false, nil // it ended between the two calls
		}
		return 0, false, err
	}
	return ct, true, nil
}

// Probe reports a pid's start time and whether it is running. ProcessStart is the real one.
type Probe func(pid int) (startMS int64, running bool, err error)

// api is the part of the container runtime's client Reap needs.
type api interface {
	ContainerList(c context.Context, o client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerRemove(c context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// Outcome is what Reap decided about one container.
type Outcome string

// The outcomes. Everything except Removed and RemoveFailed is a "kept".
const (
	Removed        Outcome = "removed: owner gone"
	RemoveFailed   Outcome = "owner gone, remove failed"
	KeptAlive      Outcome = "kept: owner alive"
	KeptOtherHost  Outcome = "kept: owner on another host"
	KeptUnreadable Outcome = "kept: owner labels missing or unreadable"
	KeptProbe      Outcome = "kept: owner could not be probed"
)

// Item is one container Reap looked at.
type Item struct {
	ID      string
	Outcome Outcome
	Detail  string
}

// Report is what one Reap looked at and did.
type Report struct {
	Namespace string
	Items     []Item
}

// Count is how many items ended in o.
func (r Report) Count(o Outcome) int {
	n := 0
	for _, it := range r.Items {
		if it.Outcome == o {
			n++
		}
	}
	return n
}

// Reap removes the containers of namespace whose owner is gone. A failure on one container is in the report, not an
// error: the rest are still reaped. Only listing can fail the call.
func Reap(c context.Context, cli api, namespace string, self Owner, probe Probe) (Report, error) {
	rep := Report{Namespace: namespace}
	res, err := cli.ContainerList(c, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", LabelNamespace+"="+namespace),
	})
	if err != nil {
		return rep, fmt.Errorf("list containers: %w", err)
	}
	for _, s := range res.Items {
		it := Item{ID: s.ID}
		it.Outcome, it.Detail = judge(s.Labels, self, probe)
		if it.Outcome == Removed {
			_, err := cli.ContainerRemove(c, s.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			switch {
			case err == nil:
			case cerrdefs.IsNotFound(err):
				// another process's reaper (or the owner itself) got there first: gone is gone
				it.Detail += " (already gone)"
			case removalInProgress(err):
				// Docker (not Podman) answers 409 when another reaper's remove of the same container is still running
				it.Detail += " (removal already in progress elsewhere)"
			default:
				it.Outcome, it.Detail = RemoveFailed, err.Error()
			}
		}
		rep.Items = append(rep.Items, it)
	}
	return rep, nil
}

// removalInProgress is the runtime's answer to a remove of a container somebody else is already removing.
//
// Only that one message counts. Docker answers a second remove of a container that is being removed with
// "removal of container <id> is already in progress" (seen in CI: Error response from daemon: removal of container
// 4feb033ec173... is already in progress); Podman answers 204 and then "no such container", the NotFound case above.
// Any other conflict, "container update is already in progress" or "cannot remove a running container" among them,
// is a real failure. The error class is not relied on: the message is what is specific.
func removalInProgress(err error) bool {
	return removalInProgressRE.MatchString(err.Error())
}

var removalInProgressRE = regexp.MustCompile(`(?:^|: )removal of container [0-9a-f]{12,64} is already in progress$`)

// judge decides from a container's labels whether its owner is gone (Removed means: remove it).
func judge(labels map[string]string, self Owner, probe Probe) (Outcome, string) {
	host, pidS, startS := labels[LabelHost], labels[LabelPID], labels[LabelStart]
	pid, perr := strconv.Atoi(pidS)
	start, serr := strconv.ParseInt(startS, 10, 64)
	if host == "" || perr != nil || serr != nil || pid <= 0 {
		return KeptUnreadable, ""
	}
	if host != self.Host {
		return KeptOtherHost, host
	}
	gotStart, running, err := probe(pid)
	switch {
	case err != nil:
		return KeptProbe, err.Error()
	case !running:
		return Removed, fmt.Sprintf("pid %d does not exist", pid)
	case gotStart != start:
		return Removed, fmt.Sprintf("pid %d was reused (start %d, owner started %d)", pid, gotStart, start)
	default:
		return KeptAlive, fmt.Sprintf("pid %d", pid)
	}
}

var (
	reapMu     sync.Mutex
	lastReport Report
	reaped     bool
)

// ReapBeforeStart reaps the namespace's orphans. It runs on EVERY container start, not once per process: an orphan that
// appears while this process is already running (another agent's test binary dies after our first scan) is removed by
// the next start, which is what "before creating a new container" in #481 means. The price is one container list per
// start, a few milliseconds against a container start of seconds; two starts at once race harmlessly (a remove that
// lost the race is not a failure, see Reap). A failure is one line on warn in a fixed format and nothing else: it never
// fails the test, and the caller's own start error is its own.
func ReapBeforeStart(c context.Context, cli api, warn io.Writer) {
	self, serr := Self()
	if serr != nil {
		self = Owner{} // no host: every owner is "another host", nothing is removed
	}
	rep, err := Reap(c, cli, Namespace(), self, ProcessStart)
	reapMu.Lock()
	lastReport, reaped = rep, err == nil
	reapMu.Unlock()
	if err != nil {
		_, _ = fmt.Fprintf(warn, "ctrreap: warning: reap skipped: %v\n", err)
	}
	for _, it := range rep.Items {
		if it.Outcome == RemoveFailed {
			_, _ = fmt.Fprintf(warn, "ctrreap: warning: could not remove %.12s: %s\n", it.ID, it.Detail)
		}
	}
}

// LastReport is what the process's latest ReapBeforeStart did, for a fixture that reports it.
func LastReport() (Report, bool) {
	reapMu.Lock()
	defer reapMu.Unlock()
	return lastReport, reaped
}

// signalGrace is how long OnSignal's terminators get in all, however many there are and however they behave.
const signalGrace = 20 * time.Second

var (
	sigMu       sync.Mutex
	terminators []func(context.Context)
	sigOnce     sync.Once
)

// OnSignal registers terminate to be run when the process gets SIGINT or SIGTERM, and installs the handler the first
// time it is called. The handler runs every registered terminator, together, under one deadline (signalGrace) that
// bounds them whatever they do, then exits with 128+signal: never 0, and never the code of anything else. A second
// signal, while it runs, ends the process at once with the same code. It does NOT signal.Reset: that is process-wide and
// would unregister the signal.Notify channels the tests under test own.
func OnSignal(terminate func(context.Context)) {
	sigMu.Lock()
	terminators = append(terminators, terminate)
	sigMu.Unlock()
	sigOnce.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		// fire-and-forget: lives as long as the test binary; it ends the process or never fires
		go handleSignal(ch, signalGrace, os.Exit)
	})
}

// handleSignal waits for one signal and then terminates and exits. grace and exit are parameters so a test can
// drive it without a real signal or a real exit.
func handleSignal(ch <-chan os.Signal, grace time.Duration, exit func(int)) {
	sig := <-ch
	code := 128 + 15
	if s, ok := sig.(syscall.Signal); ok {
		code = 128 + int(s)
	}
	c, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	// a second signal while the terminators run ends the process now
	// fire-and-forget: ends the process or never fires
	go func() { <-ch; exit(code) }()

	sigMu.Lock()
	ts := append([]func(context.Context){}, terminators...)
	sigMu.Unlock()
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Add(1)
		// fire-and-forget past the deadline: a terminator that ignores its context is abandoned, the exit below ends it
		go func() {
			defer wg.Done()
			t(c)
		}()
	}
	done := make(chan struct{})
	// fire-and-forget: only closes done
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-c.Done():
		fmt.Fprintf(os.Stderr, "ctrreap: terminating containers did not finish within %v\n", grace)
	}
	exit(code)
}
