package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// clampBudget is the arithmetic of childBudget, with the clock as arguments.
func TestClampBudget(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		deadline    time.Time
		hasDeadline bool
		want        time.Duration
		margin      time.Duration
		got         time.Duration
	}{
		"no deadline: the whole budget":                 {time.Time{}, false, 90 * time.Second, 5 * time.Second, 90 * time.Second},
		"the parent outlives the budget":                {now.Add(10 * time.Minute), true, 90 * time.Second, 5 * time.Second, 90 * time.Second},
		"the parent ends first: its rest less a margin": {now.Add(20 * time.Second), true, 90 * time.Second, 5 * time.Second, 15 * time.Second},
		"the parent ends inside the margin":             {now.Add(3 * time.Second), true, 90 * time.Second, 5 * time.Second, 0},
		"the parent is already over":                    {now.Add(-time.Second), true, 90 * time.Second, 5 * time.Second, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := clampBudget(now, tc.deadline, tc.hasDeadline, tc.want, tc.margin); got != tc.got {
				t.Fatalf("clampBudget = %s, want %s", got, tc.got)
			}
		})
	}
}

// The parent's alarm (go test -timeout) comes BEFORE the child's own budget: the child must already be
// dead when the test binary is killed, not left running behind it. The helper test starts a catalog-rpc and
// blocks forever; this test runs it under a short -timeout and checks, after the binary died, that the
// child's pid is gone.
func TestChildDoesNotOutliveItsParent(t *testing.T) {
	if os.Getenv("CHILD_DEADLINE_HELPER") != "" {
		t.Skip("the helper below is the one that runs inside the short-lived binary")
	}
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(c, "go", "test", "-count=1", "-timeout", "25s", "-run", "^TestChildHelperHoldsAChild$", ".")
	cmd.Env = append(os.Environ(), "CHILD_DEADLINE_HELPER=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := 0
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "CHILD_PID="); ok {
			pid, _ = strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	_ = cmd.Wait() // the helper binary was killed by its own -timeout: a non-zero exit is the point
	if pid == 0 {
		t.Fatal("the helper never reported its child's pid")
	}
	gone := false
	for range 30 {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			gone = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("catalog-rpc (pid %d) outlived the test binary that started it", pid)
	}
}

// TestChildHelperHoldsAChild runs only inside TestChildDoesNotOutliveItsParent's short -timeout binary.
func TestChildHelperHoldsAChild(t *testing.T) {
	if os.Getenv("CHILD_DEADLINE_HELPER") == "" {
		t.Skip("only as the helper of TestChildDoesNotOutliveItsParent")
	}
	p := startCatalogProc(t)
	println("CHILD_PID=" + strconv.Itoa(p.cmd.Process.Pid))
	select {} // until the -timeout alarm kills this binary
}
