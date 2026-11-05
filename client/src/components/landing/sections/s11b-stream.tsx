import { copy } from "../copy";
import { StreamFlow } from "../diagrams/stream-flow";
import { Section } from "../section";

export function Stream() {
  return (
    <Section stop="stream" tone="ground" labelledBy="stream-title">
      <div data-reveal="left" className="max-w-3xl">
        <h2 id="stream-title" className="t-h2">
          {copy.stream.title}
        </h2>
        <p className="t-body mt-6 max-w-[60ch]">{copy.stream.text}</p>
      </div>
      <div data-reveal="fade" className="mt-14 lg:mt-20">
        <StreamFlow />
      </div>
    </Section>
  );
}
