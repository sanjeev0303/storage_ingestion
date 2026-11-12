import { OutcomeMark, type Outcome } from "@/components/ui/status";
import { report, type Dimension } from "./copy";
import { Wrap } from "./wrap";

const OUTCOME: Record<Dimension["status"], Outcome> = {
  Passed: "passed",
  Warning: "warning",
  Failed: "failed",
  "Not checked": "not_assessed",
};

export function Report() {
  return (
    <section id="reports" className="scroll-mt-20 py-24 lg:py-32" aria-labelledby="report-title">
      <Wrap className="grid gap-x-10 gap-y-10 lg:grid-cols-12">
        <div className="lg:col-span-4">
          <h2 id="report-title" className="t-h2 orbit-stop">
            {report.heading}
          </h2>
          <p className="t-body mt-5 max-w-[30rem] text-muted">{report.intro}</p>
        </div>

        <div className="min-w-0 overflow-x-auto lg:col-span-8">
          <h3 id="report-table-title" className="table-title"><span><span className="fig-no">{report.number}</span> {report.title}</span></h3>
          <table aria-labelledby="report-table-title" className="booktabs min-w-[36rem] text-[0.9375rem]">
            <thead>
              <tr>
                {report.columns.map((c) => (
                  <th key={c} scope="col" className="first:pl-5 last:pr-5">
                    {c}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {report.dimensions.map((d) => (
                <tr key={d.name}>
                  <th scope="row" className="py-3 pr-3 pl-5 text-left align-top font-semibold">
                    {d.name}
                  </th>
                  <td className="text-muted">{d.text}</td>
                  <td>
                    <span className="inline-flex items-center gap-2 font-medium whitespace-nowrap">
                      <OutcomeMark outcome={OUTCOME[d.status]} />
                      {d.status}
                    </span>
                  </td>
                  <td className="pr-5 whitespace-nowrap">{d.coverage}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Wrap>
    </section>
  );
}
