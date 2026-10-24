import { useQuery } from "@tanstack/react-query";
import { versionQuery } from "../queries";

// Footer line: which server build the console is talking to. GET /api/version
// is unauthenticated and untraced (platform/server health), so it works before
// a credential is set and says nothing when the server is unreachable.
export function ServerVersion() {
  const { data, isError } = useQuery(versionQuery);
  if (isError) return <span className="text-destructive">server unreachable</span>;
  if (!data) return null;
  return (
    <span title={`built ${data.date} with ${data.go}`} data-testid="server-version">
      server {data.version} ({data.commit})
    </span>
  );
}
