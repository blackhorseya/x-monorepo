// Which top-level e2e/*.spec.ts run against the console image (the Playwright project "image") and which
// only against vite preview (the project "chromium", task web-e2e / console-e2e). #449.
//
// image: what only the nginx image can show.
//   image.spec.ts   nginx itself: SPA fallback, cache headers, /api proxy, /healthz, /config.json, CSP, the OIDC container.
//   version.spec.ts, account-loop.spec.ts
//                   smoke: a real browser loads each route family through nginx with no page.route in the way, so a CSP or an
//                   asset path that breaks the app is found here (image.spec.ts only reads headers for the default container).
// previewOnly: asserts UI / RPC behaviour that vite preview and nginx give identically (none of them reads a header,
// /config.json, /healthz or the CSP; most script /api with page.route, which nginx never sees). console-e2e runs them.
//
// Every top-level spec has to be in exactly one list, and the image list has to equal the set pinned in
// e2e/image-specs.check.ts, which is a copy kept on purpose: adding a spec to the image list is a decision
// that must be written down twice, in review.
export const imageSpecs = ["image.spec.ts", "version.spec.ts", "account-loop.spec.ts"];

export const previewOnlySpecs = ["admin-probe.spec.ts", "admin.spec.ts", "credential-switch.spec.ts", "transient-recovery.spec.ts"];

export interface Listed {
  /** top-level spec files (basenames) of the "chromium" project: everything classified must be in here */
  chromium: string[];
  /** spec files (basenames) the "image" project would run */
  image: string[];
}

/** Every problem with how specs are split between image and preview, empty when the split is sound. */
export function checkSpecClasses(
  listed: Listed,
  lists: { image: string[]; previewOnly: string[] },
  expectedImage: string[]
): string[] {
  const problems: string[] = [];
  const img = new Set(lists.image);
  const prev = new Set(lists.previewOnly);
  const chromium = new Set(listed.chromium);
  for (const f of chromium) {
    if (!img.has(f) && !prev.has(f)) problems.push(`${f}: not classified, add it to imageSpecs or previewOnlySpecs in e2e/image-specs.ts`);
  }
  for (const f of img) if (prev.has(f)) problems.push(`${f}: in both imageSpecs and previewOnlySpecs`);
  for (const f of [...img, ...prev]) if (!chromium.has(f)) problems.push(`${f}: listed in e2e/image-specs.ts but the chromium project has no such spec`);
  const onImage = new Set(listed.image);
  const want = new Set(expectedImage);
  for (const f of onImage) if (!want.has(f)) problems.push(`${f}: runs on the image but is not in the pinned set (e2e/image-specs.check.ts)`);
  for (const f of want) if (!onImage.has(f)) problems.push(`${f}: pinned for the image but the image project does not run it`);
  return problems;
}
