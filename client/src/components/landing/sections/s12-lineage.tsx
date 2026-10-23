import { copy } from "../copy";
import { LineageGraph } from "../diagrams/lineage-graph";
import { Section } from "../section";

export function Lineage() {
  return (
    <Section stop="lineage" tone="plum" labelledBy="lineage-title">
      <div className="grid gap-x-10 gap-y-12 lg:grid-cols-12">
        <div data-reveal="left" className="lg:col-span-5">
          <h2 id="lineage-title" className="t-h2">
            {copy.lineage.title}
          </h2>
          <p className="t-body mt-6 max-w-xl">{copy.lineage.text}</p>
          <dl className="mt-12 grid max-w-xl gap-x-8 gap-y-5 sm:grid-cols-2">
            {copy.lineage.relationNotes.map((note) => (
              <div key={note.name}>
                <dt className="font-semibold">{note.name}</dt>
                <dd className="t-body m-0 opacity-90">{note.text}</dd>
              </div>
            ))}
          </dl>
        </div>
        <div className="lg:col-span-5 lg:col-start-8">
          <LineageGraph />
        </div>
      </div>
    </Section>
  );
}
