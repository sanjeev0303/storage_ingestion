import { copy } from "../copy";
import { Section } from "../section";

export function Problem() {
  return (
    <Section stop="problem" tone="ground" labelledBy="problem-title">
      <h2 id="problem-title" className="t-h2">
        {copy.problem.title}
      </h2>
      <p data-reveal="fade" className="t-lead mt-8">
        {copy.problem.lead}
      </p>
      <div data-reveal="fade" className="mt-[72px] grid grid-cols-2 gap-x-6 gap-y-10 lg:grid-cols-4 lg:gap-x-10">
        {copy.problem.columns.map((column) => (
          <p key={column.run} className="t-body">
            <strong className="font-semibold">{column.run}</strong> {column.text}
          </p>
        ))}
      </div>
    </Section>
  );
}
