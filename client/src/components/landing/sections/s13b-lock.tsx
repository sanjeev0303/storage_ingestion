import { copy } from "../copy";
import { LockFlow } from "../diagrams/lock-flow";
import { Notes } from "../flow-kit";
import { Section } from "../section";

export function Lock() {
  return (
    <Section stop="lock" tone="ground" labelledBy="lock-title">
      <div className="grid gap-10 lg:grid-cols-12">
        <div data-reveal="left" className="lg:col-span-6">
          <h2 id="lock-title" className="t-h2">
            {copy.lock.title}
          </h2>
          <p className="t-body mt-6 max-w-[56ch]">{copy.lock.text}</p>
        </div>
        <Notes notes={copy.lock.notes} className="lg:col-span-5 lg:col-start-8 sm:grid-cols-1 lg:self-end" />
      </div>
      <div data-reveal="fade" className="mt-14 lg:mt-20">
        <LockFlow />
      </div>
    </Section>
  );
}
