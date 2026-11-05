import { copy } from "../copy";
import { AdoptFlow } from "../diagrams/adopt-flow";
import { Section } from "../section";

export function Adopt() {
  return (
    <Section stop="adopt" tone="ground" labelledBy="adopt-title">
      <div data-reveal="right" className="grid gap-6 lg:grid-cols-12">
        <h2 id="adopt-title" className="t-h2 lg:col-span-5">
          {copy.adopt.title}
        </h2>
        <p className="t-body max-w-[60ch] lg:col-span-6 lg:col-start-7">{copy.adopt.text}</p>
      </div>
      <div data-reveal="fade" className="mt-14 lg:mt-20">
        <AdoptFlow />
      </div>
      <p className="t-body mt-10 font-semibold">{copy.adopt.quota}</p>
    </Section>
  );
}
