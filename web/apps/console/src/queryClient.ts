import { QueryClient } from "@tanstack/react-query";

// One client for the app, importable outside React: the router's preload hooks run before any
// component renders and need to read what the queries already know.
export const queryClient = new QueryClient({
  defaultOptions: {
    // Domain errors (NotFound, InvalidArgument) are answers, not blips: don't retry them.
    queries: { retry: false },
  },
});
