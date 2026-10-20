/// <reference types="vite/client" />

interface ImportMetaEnv {
  // Seed for the console credential (src/auth.ts); localStorage wins once set.
  readonly VITE_CONSOLE_API_KEY?: string;
  readonly VITE_CONSOLE_TOKEN?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
