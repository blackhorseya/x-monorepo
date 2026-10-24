/// <reference types="vite/client" />

interface ImportMetaEnv {
  // Seed for the console credential (src/auth.ts); localStorage wins once set.
  readonly VITE_CONSOLE_API_KEY?: string;
  readonly VITE_CONSOLE_TOKEN?: string;
  // The release tag this build ships as (release.yml sets it); unset = "dev".
  readonly VITE_CONSOLE_VERSION?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
