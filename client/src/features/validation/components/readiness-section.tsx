import { Table, Td, Th } from "@/components/ui/table";
import { COVERAGE_LABELS, dimensionStatusOutcome } from "../status";
import { DIMENSIONS, type ReadinessReport } from "../types";
import { MappedOutcome, SectionHeading } from "./parts";

export function ReadinessSection({ readiness }: { readiness: ReadinessReport }) {
  const dimensions = [...readiness.dimensions].sort((a, b) => DIMENSIONS.indexOf(a.dimension) - DIMENSIONS.indexOf(b.dimension));
  return (
    <section aria-labelledby="readiness-heading" className="flex flex-col gap-3">
      <SectionHeading id="readiness-heading">Readiness by dimension</SectionHeading>
      <p className="max-w-prose text-base text-muted">
        Judged against <span className="mono-data">{readiness.profile}</span>. Each dimension has its own status and the coverage behind it. There is no
        overall score: a dimension that was not checked is not a pass.
      </p>
      <Table aria-label="Readiness dimensions">
        <thead>
          <tr>
            <Th>Dimension</Th>
            <Th>Status</Th>
            <Th>Coverage</Th>
            <Th>Checks</Th>
            <Th>Notes</Th>
          </tr>
        </thead>
        <tbody>
          {dimensions.map((d) => (
            <tr key={d.dimension}>
              <Td className="font-medium">{d.dimension.charAt(0) + d.dimension.slice(1).toLowerCase()}</Td>
              <Td>
                <MappedOutcome mapped={dimensionStatusOutcome(d.status)} />
              </Td>
              <Td>{COVERAGE_LABELS[d.coverage]}</Td>
              <Td className="max-w-64 whitespace-normal">
                {!d.checks || d.checks.length === 0 ? (
                  <span className="text-muted">{d.dimension === "REPRODUCIBILITY" ? "None (judged from the version itself)" : "No rule in the profile covers this"}</span>
                ) : (
                  <span className="mono-data text-sm [overflow-wrap:anywhere]">{d.checks.join(", ")}</span>
                )}
              </Td>
              <Td className="max-w-96 whitespace-normal">
                {d.notes && d.notes.length > 0 ? (
                  <ul className="list-disc pl-4">
                    {d.notes.map((note, i) => (
                      <li key={i}>{note}</li>
                    ))}
                  </ul>
                ) : (
                  <span className="text-muted">No notes</span>
                )}
              </Td>
            </tr>
          ))}
        </tbody>
      </Table>
      <figure className="neo-in-sm max-w-prose p-4">
        <figcaption className="text-sm font-semibold">From the server</figcaption>
        <p className="mt-1 text-base">{readiness.disclaimer}</p>
      </figure>
      <p className="max-w-prose text-sm text-muted">
        Dimensions have no remediation text of their own. Remediation, where the server offers it, is on individual findings below.
      </p>
    </section>
  );
}
