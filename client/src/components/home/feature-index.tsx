"use client";

import { useId, useState } from "react";
import { index } from "./home-copy";

type GroupName = (typeof index.groups)[number]["name"];

// Every feature as a real table: what it is, the problem, the fix. A row of
// chips narrows it to one stage.
export function FeatureIndex() {
  const name = useId();
  const [show, setShow] = useState<GroupName | "all">("all");
  const groups = show === "all" ? index.groups : index.groups.filter((g) => g.name === show);
  let total = 0;
  for (const g of index.groups) total += g.rows.length;
  let shown = 0;
  for (const g of groups) shown += g.rows.length;

  return (
    <section id="features" aria-labelledby="features-title" className="field-a py-24 lg:py-36">
      <div className="mx-auto max-w-[1440px] px-4 sm:px-10">
        <h2 id="features-title" data-reveal className="h-head max-w-[18ch]">
          {index.heading}
        </h2>

        <fieldset className="mt-10 min-w-0 border-0 p-0 [--chip-on:var(--h-orange)]">
          <legend className="h-small mb-3 p-0 font-medium">{index.filter}</legend>
          <div className="flex flex-wrap gap-2">
            {[{ name: "all" as const, label: index.all }, ...index.groups.map((g) => ({ name: g.name, label: g.name }))].map((o) => (
              <label key={o.name} className="h-chip h-small">
                <input type="radio" name={name} value={o.name} checked={show === o.name} onChange={() => setShow(o.name)} className="sr-only" />
                <span>{o.label}</span>
              </label>
            ))}
          </div>
        </fieldset>
        <p className="h-small mt-4 tabular-nums" aria-live="polite">
          {shown} of {total} features
        </p>

        <div data-ui className="label-cut mt-8 bg-[var(--h-tint-1)] p-4 [--cut:32px] sm:p-8">
          <table className="w-full border-collapse text-left">
            <caption className="sr-only">Features, the problem each fixes, and how</caption>
            <thead className="max-md:sr-only">
              <tr>
                <th scope="col" className="h-small w-[22%] pb-3 font-medium">
                  Feature
                </th>
                <th scope="col" className="h-small w-[30%] pb-3 font-medium">
                  The problem
                </th>
                <th scope="col" className="h-small pb-3 font-medium">
                  The fix
                </th>
              </tr>
            </thead>
            {groups.map((g) => (
              <tbody key={g.name}>
                <tr>
                  <th scope="rowgroup" colSpan={3} className="h-lead pt-8 pb-2">
                    {g.name}
                  </th>
                </tr>
                {g.rows.map((r) => (
                  <tr key={r.feature} className="border-t-2 border-[color-mix(in_srgb,currentColor_14%,transparent)] max-md:block max-md:py-3">
                    <th scope="row" className="h-body py-3 pr-6 align-top font-medium max-md:block max-md:py-0">
                      {r.feature}
                    </th>
                    <td className="h-body py-3 pr-6 align-top max-md:block max-md:py-1">{r.problem}</td>
                    <td className="h-body py-3 align-top max-md:block max-md:py-0">{r.fix}</td>
                  </tr>
                ))}
              </tbody>
            ))}
          </table>
        </div>
      </div>
    </section>
  );
}
