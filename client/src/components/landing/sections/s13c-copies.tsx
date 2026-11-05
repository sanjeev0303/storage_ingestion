import { copy } from "../copy";
import { CopiesFlow } from "../diagrams/copies-flow";
import { Section } from "../section";

export function Copies() {
  return (
    <Section stop="copies" tone="ground" labelledBy="copies-title">
      <div data-reveal="right" className="grid gap-6 lg:grid-cols-12">
        <h2 id="copies-title" className="t-h2 lg:col-span-5">
          {copy.copies.title}
        </h2>
        <p className="t-body max-w-[60ch] lg:col-span-6 lg:col-start-7">{copy.copies.text}</p>
      </div>
      <div data-reveal="fade" className="mt-14 lg:mt-20">
        <CopiesFlow />
      </div>
    </Section>
  );
}
