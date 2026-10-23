import { RetentionFlow } from "../diagrams/retention-flow";
import { copy } from "../copy";
import { Section } from "../section";
import { RetainStates } from "./retain-states";

export function Retain() {
  return (
    <Section stop="retain" tone="ground" labelledBy="retain-title">
      <div className="grid items-center gap-12 lg:grid-cols-12">
        <div data-reveal="left" className="lg:col-span-6">
          <h2 id="retain-title" className="t-h2">
            {copy.retain.title}
          </h2>
          <p className="t-lead mt-6 max-w-[34ch]">{copy.retain.text}</p>
        </div>
        <div data-reveal="right" className="lg:col-span-5 lg:col-start-8">
          <RetentionFlow />
        </div>
      </div>
      <div className="mt-20 lg:mt-28">
        <RetainStates />
      </div>
    </Section>
  );
}
