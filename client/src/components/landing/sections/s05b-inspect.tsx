import { copy } from "../copy";
import { InspectFlow } from "../diagrams/inspect-flow";
import { Notes } from "../flow-kit";
import { Section } from "../section";

export function Inspect() {
  return (
    <Section stop="inspect" tone="ground" labelledBy="inspect-title">
      <div data-reveal="left" className="max-w-3xl">
        <h2 id="inspect-title" className="t-h2">
          {copy.inspect.title}
        </h2>
        <p className="t-body mt-6 max-w-[60ch]">{copy.inspect.text}</p>
      </div>
      <div data-reveal="fade" className="mt-14 lg:mt-20">
        <InspectFlow />
      </div>
      <Notes notes={copy.inspect.notes} className="mt-14 lg:mt-20" />
    </Section>
  );
}
