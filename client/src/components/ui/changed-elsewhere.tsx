import { Button } from "./button";

// Shown on a settings form whose record someone else saved after the user
// started editing. The edits stay until the user chooses to load the latest.
export function ChangedElsewhere({ subject, onReload }: { subject: string; onReload: () => void }) {
  return (
    <div role="status" className="neo-in-sm flex flex-col items-start gap-2 p-3 text-base">
      <p className="max-w-prose">
        Someone else changed {subject} after you started editing. Your edits are kept, and saving them replaces that change.
      </p>
      <Button onClick={onReload}>Load the latest values</Button>
    </div>
  );
}
