import { Section } from "../section";
import { SnapshotStage } from "./snapshot-stage";

export function Snapshot() {
  return (
    <Section stop="snapshot" tone="ground" labelledBy="snapshot-title" bleed className="py-0 lg:py-0">
      <SnapshotStage />
    </Section>
  );
}
