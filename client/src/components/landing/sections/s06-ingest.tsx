import { copy } from "../copy";
import { IngestStates } from "../diagrams/ingest-states";
import { Container, Section } from "../section";

const { path, checksum } = copy.ingest.entry;
const manifestLine = `{"path":"${path}","size":2516582,"content_type":"image/jpeg","checksum":"${checksum}"}`;

export function Ingest() {
  return (
    <Section stop="ingest" tone="ground" bleed labelledBy="ingest-title">
      <Container>
        <div data-reveal="fade" className="grid gap-6 lg:grid-cols-3">
          <div className="lg:col-span-2">
            <h2 id="ingest-title" className="t-h2">
              {copy.ingest.title}
            </h2>
            <p className="t-body mt-6 max-w-[60ch]">{copy.ingest.text}</p>
          </div>
        </div>
        <p className="mono-data mt-10 mb-12 text-muted lg:mb-16">
          {manifestLine}
        </p>
      </Container>
      <IngestStates />
    </Section>
  );
}
