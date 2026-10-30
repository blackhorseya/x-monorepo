import { QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { loadConfig } from "./oidc/config";
import { initSession } from "./oidc/session";
import { queryClient } from "./queryClient";
import { router } from "./router";
import "./index.css";

// shadcn themes key off a .dark class; follow the OS setting.
const dark = window.matchMedia("(prefers-color-scheme: dark)");
const applyTheme = () => document.documentElement.classList.toggle("dark", dark.matches);
applyTheme();
dark.addEventListener("change", applyTheme);

// The runtime config (OIDC on/off, issuer, apiKeyLogin) decides what the first render redirects to.
await loadConfig();
await initSession();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
