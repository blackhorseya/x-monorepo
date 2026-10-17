import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Connect procedures live at /<package>.<Service>/<Method> on the Go server.
// Proxying them keeps the browser same-origin, so the server needs no CORS.
const api = process.env.CONSOLE_API ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    port: 5173,
    proxy: {
      "/ordering.v1.": api,
      "/inventory.v1.": api,
    },
  },
});
