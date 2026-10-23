import { copy } from "../copy";
import { ReadinessMenu } from "../diagrams/readiness-menu";
import { Section } from "../section";

export function Readiness() {
  return (
    <Section stop="readiness" tone="ground" labelledBy="readiness-title">
      <div className="grid gap-x-10 gap-y-6 lg:grid-cols-12">
        <h2 id="readiness-title" data-reveal="left" className="t-h2 lg:col-span-6">
          {copy.readiness.title}
        </h2>
        <div data-reveal="right" className="lg:col-span-5 lg:col-start-8">
          <p className="t-body">{copy.readiness.text}</p>
          <p className="t-body mt-3 font-semibold">{copy.readiness.noScore}</p>
        </div>
      </div>
      <div className="mt-12 lg:mt-16">
        <ReadinessMenu />
      </div>
    </Section>
  );
}
