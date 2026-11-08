// The static half of the retry guard (src/retry-overrides.test.ts): find every place in the console's
// source that sets a retry policy of its own. Pure functions over text, so the scanner itself is tested
// on fixtures (a scanner that silently finds nothing would turn the guard green for good).

export type Hit = { file: string; line: number; text: string };
export type Exception = { file: string; match: string; reason: string; test: string };

// Blank out comments, keep strings and every newline, so a hit keeps its real line number and a comment that
// talks about `retry: 3` is not a hit. A one-pass state machine: `//` inside a string is not a comment.
export function stripComments(src: string): string {
  let out = "";
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    const n = src[i + 1];
    if (c === "/" && n === "/") {
      while (i < src.length && src[i] !== "\n") (out += " "), i++;
    } else if (c === "/" && n === "*") {
      while (i < src.length && !(src[i] === "*" && src[i + 1] === "/")) (out += src[i] === "\n" ? "\n" : " "), i++;
      out += "  ";
      i += 2;
    } else if (c === '"' || c === "'" || c === "`") {
      out += c;
      i++;
      while (i < src.length && src[i] !== c) {
        if (src[i] === "\\") (out += src[i]), i++;
        out += src[i] ?? "";
        i++;
      }
      out += c;
      i++;
    } else {
      out += c;
      i++;
    }
  }
  return out;
}

// What counts as "a retry policy of its own": a `retry` / `retryDelay` property (`retry: 3`, `"retry": 3`,
// shorthand `{ retry }`, `{ retry, ... }`), an assignment to one (`x.retry = 3`), and the react-query APIs that
// change defaults behind a query's back. Property reads (`q.options.retry`) are not hits.
const PATTERNS: RegExp[] = [
  /(?<![\w$.])["']?retry(?:Delay)?["']?\s*:/,
  /(?<=[{,]\s*)retry(?:Delay)?\s*[,}]/, // shorthand: only right after `{` or `,`, so prose ("every retry, ...") is not a hit
  /\.retry(?:Delay)?\s*=(?!=)/,
  /\bnew\s+QueryClient\s*\(/,
  /\.(?:setDefaultOptions|setQueryDefaults|setMutationDefaults)\s*\(/,
];

export function scan(files: { path: string; text: string }[]): Hit[] {
  const hits: Hit[] = [];
  for (const f of files) {
    stripComments(f.text)
      .split("\n")
      .forEach((line, i) => {
        if (PATTERNS.some((p) => p.test(line))) hits.push({ file: f.path, line: i + 1, text: line.trim() });
      });
  }
  return hits;
}

export type Verdict = { problems: string[] };

// `home` is the one file allowed to set the policy (queryClient.ts). Any other hit must be named in
// `exceptions` with a reason and the test that pins its behavior; an exception that matches nothing any more,
// or that has no reason or test, is a problem too.
export function judge(hits: Hit[], exceptions: Exception[], home: string, testExists: (path: string) => boolean): Verdict {
  const problems: string[] = [];
  for (const e of exceptions) {
    if (e.reason.trim() === "") problems.push(`exception ${e.file} "${e.match}": no reason`);
    if (e.test.trim() === "") problems.push(`exception ${e.file} "${e.match}": no test`);
    else if (!testExists(e.test)) problems.push(`exception ${e.file} "${e.match}": test ${e.test} does not exist`);
    if (!hits.some((h) => h.file === e.file && h.text.includes(e.match))) {
      problems.push(`exception ${e.file} "${e.match}": matches nothing any more, remove it`);
    }
  }
  for (const h of hits) {
    if (h.file === home) continue;
    if (!exceptions.some((e) => e.file === h.file && h.text.includes(e.match))) {
      problems.push(`${h.file}:${h.line}: ${h.text}  (a retry policy outside ${home}; use the shared one or add an exception with a reason and a test)`);
    }
  }
  return { problems };
}
