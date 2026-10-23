import { copy } from "../copy";
import { ReportPanel } from "../diagrams/report-panel";
import { Section } from "../section";

type NoteProps = { head: string; text: string; side: "left" | "right"; className: string };

function Note({ head, text, side, className }: NoteProps) {
  return (
    <div data-reveal={side} className={className}>
      <p className="t-body font-semibold">{head}</p>
      <p className="t-body mt-1">{text}</p>
    </div>
  );
}

// Panel first on phones, with the notes under it; on large screens the notes sit
// in the margins beside the part of the panel they discuss.
export function Report() {
  const { left, right } = copy.report.notes;

  return (
    <Section stop="report" tone="ground" labelledBy="report-title">
      <h2 id="report-title" className="t-h2 max-w-[20ch]">
        {copy.report.title}
      </h2>

      <div className="mt-14 grid gap-x-10 gap-y-8 sm:grid-cols-2 lg:mt-20 lg:grid-cols-[1fr_minmax(440px,480px)_1fr] lg:grid-rows-[3fr_2fr]">
        <ReportPanel className="sm:col-span-2 lg:col-span-1 lg:col-start-2 lg:row-span-2 lg:row-start-1" />
        <Note {...left[0]} side="left" className="lg:col-start-1 lg:row-start-1 lg:pt-28" />
        <Note {...left[1]} side="left" className="lg:col-start-1 lg:row-start-2" />
        <Note {...right[0]} side="right" className="lg:col-start-3 lg:row-start-1 lg:pt-28" />
        <Note {...right[1]} side="right" className="lg:col-start-3 lg:row-start-2" />
      </div>
    </Section>
  );
}
