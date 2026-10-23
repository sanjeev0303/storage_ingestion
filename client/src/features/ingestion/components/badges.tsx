import { StateBadge } from "@/components/ui/status";
import { entryLabel, entryTone, jobLabel, jobTone } from "../lib/explain";
import type { EntryState, JobState } from "../types";

export function JobBadge({ state }: { state: JobState }) {
  return <StateBadge tone={jobTone(state)}>{jobLabel(state)}</StateBadge>;
}

export function EntryBadge({ state }: { state: EntryState }) {
  return <StateBadge tone={entryTone(state)}>{entryLabel(state)}</StateBadge>;
}
