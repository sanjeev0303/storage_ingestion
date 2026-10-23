import { Panel } from "@/components/ui/panel";

export function DraftVsSnapshot() {
  return (
    <Panel tone="in" title="Draft and snapshots" className="mb-6">
      <div className="grid gap-4 text-base sm:grid-cols-2">
        <div>
          <h3 className="font-semibold">The dataset is a draft</h3>
          <p className="mt-1 text-muted">
            Members, labels and splits can change at any time. Every change, and every new snapshot, moves a dataset
            that was Ready or Failed back to Draft.
          </p>
        </div>
        <div>
          <h3 className="font-semibold">A snapshot is a version, and it never changes</h3>
          <p className="mt-1 text-muted">
            A snapshot pins the exact object version of every member and records a manifest hash. Editing the draft
            afterwards does not touch any existing version. Validate and train on a version, not on the draft.
          </p>
        </div>
      </div>
    </Panel>
  );
}

export function ImmutableBadge() {
  return (
    <span className="inline-flex items-center rounded-sm bg-ink px-2 py-0.5 text-xs font-semibold whitespace-nowrap text-ground">
      Immutable
    </span>
  );
}

export function DraftBadge() {
  return (
    <span className="neo-in-sm inline-flex items-center px-2 py-0.5 text-xs font-semibold whitespace-nowrap">
      Mutable draft
    </span>
  );
}
