import { Outcome } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import type { TestCategory, TestCheckName, TestResponse } from "../types";

const CHECK_LABELS: Record<TestCheckName, string> = {
  endpoint: "Endpoint reachable and allowed",
  credentials: "Credentials accepted",
  bucket: "Bucket or container found",
  list: "Objects can be listed",
  existing_object: "An existing object can be read",
  write_probe: "Write, read back and delete a test object",
};

// The backend never sends provider text, only a category; these are its fixed messages.
export const CATEGORY_MESSAGES: Record<TestCategory, string> = {
  ACCESS_DENIED: "The credentials are not allowed to do this; check the permissions in docs/storage-connections.md.",
  NOT_FOUND: "The bucket or container does not exist, or the credentials cannot see it.",
  THROTTLED: "The provider is throttling requests; try again later.",
  NETWORK: "The endpoint could not be reached in time.",
  ENDPOINT_REJECTED: "The endpoint is not allowed by the platform's network policy.",
  TLS: "The endpoint's TLS certificate is not trusted.",
  INVALID_CREDENTIALS: "The credentials are malformed or could not be used.",
  UNSUPPORTED: "The provider does not support an operation the platform needs.",
  PROVIDER_ERROR: "The provider returned an unexpected error.",
};

// ok:false is a failed test even though the HTTP status was 200.
export function TestResult({ result, note }: { result: TestResponse; note?: string }) {
  return (
    <div role="status" className="neo-in-sm flex flex-col gap-3 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Outcome outcome={result.ok ? "passed" : "failed"} label={result.ok ? "Test passed" : "Test failed"} />
        <span className="text-sm text-muted">
          Tested <Time value={result.tested_at} />
        </span>
      </div>
      {!result.ok && (
        <p className="text-base">
          {result.category && <span className="mono-data font-semibold">{result.category}</span>}
          {result.category && result.message ? ": " : ""}
          {result.message}
        </p>
      )}
      {note && <p className="text-base font-medium">{note}</p>}
      {result.checks.length > 0 && (
        <ul className="flex flex-col gap-1.5">
          {result.checks.map((check) => (
            <li key={check.name} className="flex flex-wrap items-center gap-x-3 text-base">
              <Outcome outcome={check.ok ? "passed" : "failed"} label={CHECK_LABELS[check.name] ?? check.name} />
              {!check.ok && check.category && <span className="mono-data text-sm text-muted">{check.category}</span>}
            </li>
          ))}
        </ul>
      )}
      {!result.ok && result.checks.length > 0 && (
        <p className="text-sm text-muted">Checks stop at the first failure, so later checks did not run.</p>
      )}
    </div>
  );
}
