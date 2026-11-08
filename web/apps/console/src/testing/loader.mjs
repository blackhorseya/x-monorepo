// Resolve hook for the console's node tests. Three jobs, nothing else:
//  1. vite-style extensionless relative imports ("./transient", and the generated "@repo/proto" files'
//     "../../common/v1/money_pb") get the .ts / .tsx they stand for;
//  2. TypeScript is transformed by vite's own transformWithOxc (the console's build tool, so no new
//     dependency and the same TS the build sees). Stripping alone is not enough: `enum` is not erasable
//     and the generated @repo/proto files are full of them; node's transform mode varies by version;
//  3. src/client.ts (createConnectTransport, window.location, the OIDC session) is swapped for
//     src/testing/client.stub.ts, so the real queryFn of every query runs against a scripted server.
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const CLIENT_STUB = new URL("./client.stub.ts", import.meta.url).href;

export async function resolve(specifier, context, nextResolve) {
  if (specifier.startsWith(".") && context.parentURL?.startsWith("file:")) {
    const target = new URL(specifier, context.parentURL);
    if (/\/src\/client$/.test(target.pathname)) return { url: CLIENT_STUB, shortCircuit: true };
    if (!/\.[cm]?[jt]sx?$/.test(target.pathname)) {
      for (const ext of [".ts", ".tsx"]) {
        const candidate = new URL(target.href + ext);
        if (existsSync(fileURLToPath(candidate))) return { url: candidate.href, shortCircuit: true };
      }
    }
  }
  return nextResolve(specifier, context);
}

export async function load(url, context, nextLoad) {
  if (!/\.tsx?$/.test(url) || !url.startsWith("file:")) return nextLoad(url, context);
  const { transformWithOxc } = await import("vite");
  const path = fileURLToPath(url);
  const out = await transformWithOxc(readFileSync(path, "utf8"), path, { sourcemap: false });
  return { format: "module", source: out.code, shortCircuit: true };
}
