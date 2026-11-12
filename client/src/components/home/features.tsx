import {
  ClipboardText,
  Files,
  HardDrives,
  Key,
  LockSimple,
  SealCheck,
  ShieldCheck,
  Stack,
  UploadSimple,
} from "@phosphor-icons/react/dist/ssr";
import type { Icon } from "@phosphor-icons/react";
import { features } from "./copy";
import { Wrap } from "./wrap";

const ICONS: Record<string, Icon> = {
  storage: HardDrives,
  ingest: UploadSimple,
  gate: ShieldCheck,
  catalog: Files,
  datasets: Stack,
  readiness: SealCheck,
  governance: LockSimple,
  evidence: ClipboardText,
  access: Key,
};

export function Features() {
  return (
    <section id="features" className="scroll-mt-20 border-t border-line py-24 lg:py-32" aria-labelledby="features-title">
      <Wrap>
        <div className="grid gap-x-10 gap-y-5 lg:grid-cols-12">
          <h2 id="features-title" className="t-h2 orbit-stop lg:col-span-6">
            {features.heading}
          </h2>
          <p className="t-body max-w-[36rem] text-muted lg:col-span-5 lg:col-start-8 lg:self-end">{features.intro}</p>
        </div>

        <div className="mt-12">
          <h3 id="features-table-title" className="table-title"><span><span className="fig-no">{features.number}</span> {features.title}</span></h3>
          <table aria-labelledby="features-table-title" className="booktabs text-[0.9375rem] max-md:block">
            <thead className="max-md:sr-only">
              <tr>
                {features.columns.map((c, i) => (
                  <th key={c} scope="col" className={i === 0 ? "w-[26%] pl-5" : i === 2 ? "w-[38%] pr-5" : ""}>
                    {c}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="max-md:block">
              {features.rows.map((row) => {
                const RowIcon = ICONS[row.id];
                return (
                  <tr key={row.id} className="max-md:block max-md:border-b max-md:border-line max-md:px-5 max-md:py-4 max-md:last:border-b-2 max-md:last:border-ink">
                    <th scope="row" className="py-3.5 pr-3 pl-5 text-left align-top max-md:block max-md:border-0 max-md:p-0">
                      <span className="inline-flex items-center gap-3 font-semibold">
                        <span className="grid size-8 shrink-0 place-items-center rounded-md bg-ultra-soft text-ultra-text">
                          <RowIcon aria-hidden className="size-[18px]" weight="bold" />
                        </span>
                        {row.name}
                      </span>
                    </th>
                    <td className="text-muted max-md:block max-md:border-0 max-md:px-0 max-md:pt-2 max-md:pb-1">{row.fixes}</td>
                    <td className="pr-5 max-md:block max-md:border-0 max-md:p-0">
                      <ul className="flex flex-col gap-1">
                        {row.features.map((f) => (
                          <li key={f} className="flex gap-2">
                            <span aria-hidden className="mt-[0.55em] size-1.5 shrink-0 rounded-full bg-ultra-text" />
                            {f}
                          </li>
                        ))}
                      </ul>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Wrap>
    </section>
  );
}
