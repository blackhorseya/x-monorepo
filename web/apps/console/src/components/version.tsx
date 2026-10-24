import { useQuery } from "@tanstack/react-query";
import { versionQuery } from "../queries";

// Footer line: this console build and the server build it is talking to. The console's
// version is baked in by release.yml (VITE_CONSOLE_VERSION = the tag; "dev" otherwise);
// the server's comes from GET /api/version, unauthenticated and untraced (platform/server
// health), so it works before a credential is set and says nothing when the server is
// unreachable. Two real versions that differ get flagged: the image and the rpc are
// released together, so a mismatch means a half-rolled deploy.
const consoleVersion = import.meta.env.VITE_CONSOLE_VERSION || "dev";

export function ServerVersion() {
  const { data, isError } = useQuery(versionQuery);
  if (isError)
    return <span className="text-destructive">server unreachable</span>;
  if (!data) return null;
  const mismatch =
    consoleVersion !== "dev" &&
    data.version !== "dev" &&
    consoleVersion !== data.version;
  return (
    <span
      title={`server built ${data.date} with ${data.go}`}
      data-testid="server-version"
    >
      console {consoleVersion} · server {data.version} ({data.commit})
      {mismatch && (
        <span className="text-destructive"> — version mismatch</span>
      )}
    </span>
  );
}
