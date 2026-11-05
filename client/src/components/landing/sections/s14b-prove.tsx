import { copy } from "../copy";
import { ProveFlow } from "../diagrams/prove-flow";
import { Notes } from "../flow-kit";
import { Section } from "../section";

export function Prove() {
  return (
    <Section stop="prove" tone="ground" labelledBy="prove-title">
      <div data-reveal="left" className="max-w-3xl">
        <h2 id="prove-title" className="t-h2">
          {copy.prove.title}
        </h2>
        <p className="t-body mt-6 max-w-[60ch]">{copy.prove.text}</p>
      </div>
      <div data-reveal="fade" className="mt-14 lg:mt-20">
        <ProveFlow />
      </div>
      <Notes notes={copy.prove.exports} className="mt-14 lg:mt-20" />
    </Section>
  );
}
