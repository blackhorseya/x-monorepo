import { execFile, spawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { promisify } from "node:util";

// The only module of the e2e selftests that starts a process or waits for one to end (#479, #484). A caller gets a
// `Tracked`, never the ChildProcess: it cannot listen for `exit`, so there is no unbounded wait to write. Every wait is
// `waitChild(t, ms)`: exit, error (a spawn that failed emits `error` and never `exit`) or timeout, whichever is first.
//
// Public API, and what each can and cannot do:
//   spawnTracked(command, {cwd})  starts a child in a group of its own. The ONLY way to get a Tracked that can be signalled.
//                                 Cannot: take a shell string, inherit stdio, or hand out the ChildProcess.
//   waitChild(t, ms)              waits for exit | error | timeout, 0..MAX_WAIT_MS. Cannot wait without a limit
//                                 (NaN, negative, Infinity and > MAX_WAIT_MS throw RangeError).
//   Tracked (type only)           `pid` and `ended` (what happened, once it has). Cannot: be constructed outside this
//                                 module, expose a Promise, a handle, or `once`/`on`.
//   signalGroup(t, signal)        signals the group of a Tracked that spawnTracked made. Cannot signal any other pid:
//                                 anything not made by spawnTracked is refused (TypeError).
//   groupAlive(t)                 whether that group still has a member. Same restriction.
//   lastSpawned()                 the newest Tracked that spawnTracked made.
//   reapAll(ms)                   SIGKILL every group spawnTracked made, then waitChild each for at most ms.
//   runBounded(file, args, opts)  runs a command to its end for 1..MAX_WAIT_MS ms (then SIGKILL): code, stdout+stderr.
//   MAX_WAIT_MS, ChildEnd         the limit, and the result type of waitChild.
//   __testing.fake(child)         a Tracked over a fake EventEmitter, for unit-testing waitChild. Not signal-able.
//   __testing.neverEnds(t)        a Tracked over the same group as a spawned t that never reports its end, for the "no
//                                 end event" branch. Signals go to t's own group only; it is not a way to reach another pid.

/** How a child ended, or that it did not within the wait. */
export type ChildEnd = { kind: "exit"; code: number | null; signal: NodeJS.Signals | null } | { kind: "error"; error: Error } | { kind: "timeout" };
/** What `track` needs of a child; a real ChildProcess is one, so is a fake EventEmitter in a unit test. */
type ChildLike = EventEmitter & { pid?: number; exitCode: number | null; signalCode: NodeJS.Signals | null };
const handles = new WeakMap<Tracked, ChildLike>();
const spawned: Tracked[] = [];

/**
 * A child that is being watched. It exposes no Promise and no handle: what has happened (`ended`) and `pid`, nothing a
 * caller could `await` without a limit. The only way to wait is `waitChild(t, ms)`.
 */
class Tracked {
  readonly pid: number | undefined;
  #ended: ChildEnd | undefined;
  readonly #whenEnded: Promise<ChildEnd>;
  constructor(child: ChildLike) {
    this.pid = child.pid;
    // Hooks `exit` and `error` NOW (an `error` with no listener is an uncaught exception), so that no wait has to come first.
    this.#whenEnded = new Promise<ChildEnd>((resolve) => {
      const end = (e: ChildEnd) => resolve((this.#ended ??= e));
      child.once("exit", (code: number | null, signal: NodeJS.Signals | null) => end({ kind: "exit", code, signal }));
      child.once("error", (error: Error) => end({ kind: "error", error }));
    });
    handles.set(this, child);
  }
  /** How it ended, once it has (the first of exit and error); undefined while it runs. */
  get ended(): ChildEnd | undefined {
    return this.#ended;
  }
  /** Resolves as soon as the child exits, fails to spawn, or `ms` have passed (`timeout`). Never longer than `ms`. */
  static wait(t: Tracked, ms: number): Promise<ChildEnd> {
    if (!(ms >= 0 && ms <= MAX_WAIT_MS)) throw new RangeError(`a wait is 0..${MAX_WAIT_MS} ms, got ${ms}`);
    if (t.#ended) return Promise.resolve(t.#ended);
    let timer: ReturnType<typeof setTimeout> | undefined;
    return Promise.race([t.#whenEnded, new Promise<ChildEnd>((r) => (timer = setTimeout(() => r({ kind: "timeout" }), ms)))]).finally(() => clearTimeout(timer));
  }
}
/** The longest wait or run this module accepts (setTimeout treats anything past 2^31-1 ms as 1 ms: that is not "longer"). */
export const MAX_WAIT_MS = 10 * 60_000;

export type { Tracked };
/** Tracked objects that spawnTracked made: the only ones signalGroup, groupAlive and reapAll act on. */
const real = new WeakSet<Tracked>();
export const waitChild = (t: Tracked, ms: number): Promise<ChildEnd> => Tracked.wait(t, ms);

/** Starts `command` in a group of its own, tracked from the first tick and reaped by `reapAll` whatever the caller did. */
export function spawnTracked(command: readonly string[], options: { cwd?: string } = {}): Tracked {
  const t = new Tracked(spawn(command[0]!, command.slice(1), { cwd: options.cwd, stdio: "ignore", detached: true }));
  real.add(t);
  spawned.push(t);
  return t;
}

function ownChild(t: Tracked): ChildLike {
  if (!real.has(t)) throw new TypeError("not a child that spawnTracked started: refusing to signal it");
  return handles.get(t)!;
}

/** Signals the group of a child this module started (`pnpm exec` starts vite as ITS child: the group, or vite outlives the test). */
export function signalGroup(t: Tracked, signal: NodeJS.Signals): void {
  const c = ownChild(t);
  try {
    if (c.pid) process.kill(-c.pid, signal);
  } catch (e) {
    // ESRCH: the group is gone. macOS answers EPERM for a group whose members have all ended but are not yet
    // reaped, so EPERM counts as gone only once the child we started has ended; anything else is a real failure.
    const code = (e as NodeJS.ErrnoException).code;
    const gone = c.exitCode !== null || c.signalCode !== null;
    if (code !== "ESRCH" && !(code === "EPERM" && gone)) throw e;
  }
}

/** Whether anything of the child's group is still alive. */
export function groupAlive(t: Tracked): boolean {
  const c = ownChild(t);
  if (!c.pid) return false;
  try {
    process.kill(-c.pid, 0);
    return true;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === "EPERM" && c.exitCode === null;
  }
}

/** The children started so far, newest last. */
export const lastSpawned = (): Tracked | undefined => spawned[spawned.length - 1];

/** SIGKILL every group started here, then wait (at most `ms` each) until each child is reaped. */
export async function reapAll(ms: number): Promise<void> {
  spawned.forEach((t) => signalGroup(t, "SIGKILL"));
  await Promise.all(spawned.map((t) => waitChild(t, ms)));
}

const execFileAsync = promisify(execFile);
/** Runs `file args` to its end, for at most `timeoutMs` (then SIGKILL): exit code and stdout+stderr. */
export async function runBounded(file: string, args: string[], options: { cwd?: string; env?: NodeJS.ProcessEnv; timeoutMs: number }): Promise<{ code: number; out: string }> {
  if (!(options.timeoutMs > 0 && options.timeoutMs <= MAX_WAIT_MS)) throw new RangeError(`a run is 1..${MAX_WAIT_MS} ms, got ${options.timeoutMs}`);
  try {
    const { stdout, stderr } = await execFileAsync(file, args, { cwd: options.cwd, env: options.env, timeout: options.timeoutMs, killSignal: "SIGKILL" });
    return { code: 0, out: stdout + stderr };
  } catch (e) {
    const err = e as { code?: number; stdout?: string; stderr?: string };
    return { code: typeof err.code === "number" ? err.code : -1, out: `${err.stdout ?? ""}${err.stderr ?? ""}` };
  }
}

/** For the unit tests of this module's own waits; neither can reach a process that spawnTracked did not start. */
export const __testing = {
  fake: (child: ChildLike): Tracked => new Tracked(child), // not in `real`: signalGroup refuses it
  neverEnds: (t: Tracked): Tracked => {
    const c = ownChild(t);
    const silent = new Tracked(Object.assign(new EventEmitter(), { pid: c.pid, exitCode: null, signalCode: null })); // hooks nothing of c
    handles.set(silent, c);
    real.add(silent);
    return silent;
  },
};
