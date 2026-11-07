import { execFile, spawn } from "node:child_process";
import type { EventEmitter } from "node:events";
import { promisify } from "node:util";

// The only module of the e2e selftests that starts a process or waits for one to end (#479, #484). A caller gets a
// `Tracked`, never the ChildProcess: it cannot listen for `exit`, so there is no unbounded wait to write. Every wait is
// `waitChild(t, ms)`: exit, error (a spawn that failed emits `error` and never `exit`) or timeout, whichever is first.

/** How a child ended, or that it did not within the wait. */
export type ChildEnd = { kind: "exit"; code: number | null; signal: NodeJS.Signals | null } | { kind: "error"; error: Error } | { kind: "timeout" };
/** What `track` needs of a child; a real ChildProcess is one, so is a fake EventEmitter in a unit test. */
export type ChildLike = EventEmitter & { pid?: number; exitCode: number | null; signalCode: NodeJS.Signals | null };
export type Tracked = { readonly pid: number | undefined; ended?: ChildEnd; readonly whenEnded: Promise<ChildEnd> };

const handles = new WeakMap<Tracked, ChildLike>();
const spawned: Tracked[] = [];

/** Hooks `exit` and `error` NOW (an `error` with no listener is an uncaught exception), so that no wait has to come first. */
export function track(child: ChildLike): Tracked {
  let resolveEnd!: (e: ChildEnd) => void;
  const t: Tracked = { pid: child.pid, whenEnded: new Promise<ChildEnd>((r) => (resolveEnd = r)) };
  const end = (e: ChildEnd) => resolveEnd((t.ended ??= e));
  child.once("exit", (code: number | null, signal: NodeJS.Signals | null) => end({ kind: "exit", code, signal }));
  child.once("error", (error: Error) => end({ kind: "error", error }));
  handles.set(t, child);
  return t;
}

/** Resolves as soon as the child exits, fails to spawn, or `ms` have passed (`timeout`). Never longer than `ms`. */
export function waitChild(t: Tracked, ms: number): Promise<ChildEnd> {
  if (t.ended) return Promise.resolve(t.ended);
  let timer: ReturnType<typeof setTimeout> | undefined;
  return Promise.race([t.whenEnded, new Promise<ChildEnd>((r) => (timer = setTimeout(() => r({ kind: "timeout" }), ms)))]).finally(() => clearTimeout(timer));
}

/** Starts `command` in a group of its own, tracked from the first tick and reaped by `reapAll` whatever the caller did. */
export function spawnTracked(command: readonly string[], options: { cwd?: string } = {}): Tracked {
  const t = track(spawn(command[0]!, command.slice(1), { cwd: options.cwd, stdio: "ignore", detached: true }));
  spawned.push(t);
  return t;
}

/** Signals the group of a child this module started (`pnpm exec` starts vite as ITS child: the group, or vite outlives the test). */
export function signalGroup(t: Tracked, signal: NodeJS.Signals): void {
  const c = handles.get(t)!;
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
  const c = handles.get(t)!;
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
  try {
    const { stdout, stderr } = await execFileAsync(file, args, { cwd: options.cwd, env: options.env, timeout: options.timeoutMs, killSignal: "SIGKILL" });
    return { code: 0, out: stdout + stderr };
  } catch (e) {
    const err = e as { code?: number; stdout?: string; stderr?: string };
    return { code: typeof err.code === "number" ? err.code : -1, out: `${err.stdout ?? ""}${err.stderr ?? ""}` };
  }
}
