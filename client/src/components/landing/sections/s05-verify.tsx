import { copy } from "../copy";
import { DiagramFigure } from "../diagram-kit";
import { VerifyPipeline } from "../diagrams/verify-pipeline";
import { Container, Section } from "../section";

export function Verify() {
  return (
    <Section stop="verify" tone="plum" bleed labelledBy="verify-title">
      <Container>
        <div data-reveal="left" className="grid gap-6 lg:grid-cols-3">
          <div>
            <h2 id="verify-title" className="t-h2">
              {copy.verify.title}
            </h2>
            <p className="t-body mt-6">{copy.verify.text}</p>
          </div>
        </div>
        <div className="mt-16 lg:mt-20">
          <DiagramFigure
            label={copy.verify.diagramLabel}
            className="hidden md:block"
          >
            <VerifyPipeline orientation="row" />
          </DiagramFigure>
          <DiagramFigure
            label={copy.verify.diagramLabel}
            className="mx-auto max-w-sm md:hidden"
          >
            <VerifyPipeline orientation="column" />
          </DiagramFigure>
        </div>
      </Container>
    </Section>
  );
}
