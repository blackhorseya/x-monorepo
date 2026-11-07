import { type ChildProcess, spawn } from "node:child_process";

// Runs a nested `node --test` to its end for at most `deadlineMs`, in a group of its own. The two selftests that run
// the whole selftest inside another test use this and nothing else: a nested run that hangs (a vite preview that
// never answers) is killed — the whole group, or its vite outlives it — and waited for, instead of holding the
// parent until the parent's own timeout takes it down with the child still running (#479).

// `closeTimedOut`: the child exited but its stdout/stderr did not close within `closeMs` (something it started still holds them), so `out` may be incomplete.
export type NestedResult = { code: number | null; signal: NodeJS.Signals | null; out: string; timedOut: boolean; closeTimedOut: boolean };
const MAX_DEADLINE_MS = 10 * 60_000;
const live = new Set<ChildProcess>();

function killGroup(c: ChildProcess): void {
  try {
    if (c.pid) process.kill(-c.pid, "SIGKILL");
  } catch (e) {
    // ESRCH: the group is gone. macOS answers EPERM for a group whose members all exited but are not yet reaped,
    // so EPERM counts as gone only once the child we started has exited; anything else is a real failure.
    const code = (e as NodeJS.ErrnoException).code;
    const exited = c.exitCode !== null || c.signalCode !== null;
    if (code !== "ESRCH" && !(code === "EPERM" && exited)) throw e;
  }
}

/** Whatever a test left running: SIGKILL every group started here. For `after()`. */
export function killNested(): void {
  live.forEach(killGroup);
}

/** True while the group of the nested run still has a member. */
export function groupAlive(pid: number): boolean {
  try {
    process.kill(-pid, 0);
    return true;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === "EPERM";
  }
}

/**
 * `node args…` in `cwd` with `env`. Resolves when it exits, or `deadlineMs` after the start: then the group is
 * SIGKILLed and waited for (at most `reapMs`) and `timedOut` is true. A spawn that fails rejects (it emits `error`, never `exit`).
 */
export function runNested(args: string[], options: { cwd: string; env: NodeJS.ProcessEnv; deadlineMs: number; reapMs?: number; closeMs?: number; onSpawn?: (pid: number) => void }): Promise<NestedResult> {
  const { deadlineMs, reapMs = 5_000 } = options;
  if (!(deadlineMs > 0 && deadlineMs <= MAX_DEADLINE_MS)) throw new RangeError(`a nested run is 1..${MAX_DEADLINE_MS} ms, got ${deadlineMs}`);
  return new Promise((resolve, reject) => {
    // Inside `node --test`, NODE_TEST_CONTEXT makes a nested `node --test` run quietly in-process and exit 0 without
    // running anything: a passing control that tested nothing. Strip it; callers check the run really ran tests.
    const { NODE_TEST_CONTEXT: _context, ...env } = options.env;
    const child = spawn("node", args, { cwd: options.cwd, env, stdio: ["ignore", "pipe", "pipe"], detached: true });
    live.add(child);
    if (child.pid) options.onSpawn?.(child.pid);
    let out = "";
    let timedOut = false;
    let settled = false;
    const finish = (code: number | null, signal: NodeJS.Signals | null) => {
      if (settled) return;
      settled = true;
      clearTimeout(deadline);
      clearTimeout(giveUp);
      live.delete(child);
      resolve({ code, signal, out, timedOut, closeTimedOut: false });
    };
    child.stdout!.on("data", (d) => (out += d));
    child.stderr!.on("data", (d) => (out += d));
    child.once("error", (e) => {
      settled = true;
      clearTimeout(deadline);
      clearTimeout(giveUp);
      live.delete(child);
      reject(e);
    });
    child.once("exit", (code, signal) => finish(code, signal));
    let giveUp: ReturnType<typeof setTimeout> | undefined;
    const deadline = setTimeout(() => {
      timedOut = true;
      killGroup(child);
      // The exit of a SIGKILLed child is reported at once; if it is not, say so rather than wait for ever.
      giveUp = setTimeout(() => finish(child.exitCode, child.signalCode), reapMs);
    }, deadlineMs);
  });
}
