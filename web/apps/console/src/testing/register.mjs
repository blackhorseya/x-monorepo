// Registered with `node --import ./src/testing/register.mjs` (task web-test): lets `node --test` load the
// console's own modules, which are written for vite (extensionless relative imports, a browser-only client).
import { register } from "node:module";

register("./loader.mjs", import.meta.url);
