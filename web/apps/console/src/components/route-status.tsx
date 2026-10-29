import { useRouter, type ErrorComponentProps } from "@tanstack/react-router";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";

// Shown while a lazy route chunk (or its loader) is in flight. Routes are code-split, so the
// first visit to a page pays one chunk fetch; the layout around it stays rendered.
export function RoutePending() {
  return (
    <p role="status" className="text-sm text-muted-foreground">
      Loading…
    </p>
  );
}

// Shown when a route fails to load, most often a chunk that 404s after a redeploy or a dropped
// connection. Retry re-runs the dynamic import; without this a failed chunk would be a blank page.
export function RouteError({ error, reset }: ErrorComponentProps) {
  const router = useRouter();
  return (
    <Alert variant="destructive">
      <AlertTitle>Failed to load this page</AlertTitle>
      <AlertDescription>
        <p>{error instanceof Error ? error.message : String(error)}</p>
        <Button
          variant="outline"
          size="sm"
          className="mt-2"
          onClick={() => {
            reset();
            void router.invalidate();
          }}
        >
          Retry
        </Button>
      </AlertDescription>
    </Alert>
  );
}
