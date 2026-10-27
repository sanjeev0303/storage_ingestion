"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef } from "react";
import { Outcome, type Outcome as OutcomeName } from "@/components/ui/status";
import { cn } from "@/lib/utils/cn";

gsap.registerPlugin(useGSAP, ScrollTrigger);

const checks: { name: string; outcome: OutcomeName; detail?: string }[] = [
  {
    name: "Sizes match the manifest",
    outcome: "passed",
    detail: "1,204 of 1,204 match",
  },
  {
    name: "Content integrity",
    outcome: "partial",
    detail: "Sampled 100 of 1,204",
  },
  {
    name: "Duplicate content",
    outcome: "warning",
    detail: "3 groups of identical files",
  },
  {
    name: "Required labels",
    outcome: "failed",
    detail: "Missing on 9 members",
  },
  {
    name: "Malware scan",
    outcome: "not_assessed",
    detail: "No scan result for 12 members",
  },
];

const evidence = [
  ["Checksums verified", "1,204 of 1,204"],
  ["Scans clean", "1,192"],
  ["Scans with no result", "12"],
  ["Content read", "100 members"],
  ["Content not read", "1,104 members"],
  ["Bytes read", "412 MiB"],
];

const splits = [
  ["train", "843"],
  ["validation", "181"],
  ["test", "180"],
];

// The validation report at true size. Rows resolve in order as it scrolls in.
export function ReportPanel({ className }: { className?: string }) {
  const root = useRef<HTMLElement>(null);

  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        const tl = gsap.timeline({
          defaults: { ease: "power2.out" },
          scrollTrigger: {
            trigger: root.current,
            start: "top 80%",
            end: "bottom 55%",
            scrub: 0.5,
          },
        });
        tl.from("[data-row]", {
          autoAlpha: 0,
          y: 10,
          duration: 0.4,
          stagger: 0.09,
        });
        tl.from(
          "[data-row] svg",
          {
            scale: 0.6,
            transformOrigin: "50% 50%",
            duration: 0.3,
            stagger: 0.09,
          },
          0.1,
        );
      });
    },
    { scope: root },
  );

  return (
    <article
      ref={root}
      data-ui
      data-lenis-prevent-horizontal=""
      aria-labelledby="report-panel-title"
      className={cn(
        "w-full max-w-[480px] justify-self-center overflow-x-auto bg-faint p-5 text-ink",
        className,
      )}
    >
      <div className="min-w-[280px]">
        <h3 id="report-panel-title" className="t-body font-semibold">
          Report
        </h3>
        <dl className="t-small mt-3 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
          <dt className="text-muted">Profile</dt>
          <dd className="mono-data">builtin:general v1</dd>
          <dt className="text-muted">Mode</dt>
          <dd>Sampled</dd>
          <dt className="text-muted">Verdict</dt>
          <dd data-row className="font-semibold">
            <Outcome outcome="failed" label="FAILED" />
          </dd>
        </dl>

        <ul data-taste-skill-ignore className="mt-5 border-t border-ink/15">
          {checks.map((check) => (
            <li
              key={check.name}
              data-row
              data-taste-skill-ignore className="flex items-start justify-between gap-4 border-b border-ink/15 py-3"
            >
              <div>
                <p className="t-body font-medium">{check.name}</p>
                {check.detail && (
                  <p className="t-small text-muted">{check.detail}</p>
                )}
              </div>
              <span className="t-small pt-1">
                <Outcome outcome={check.outcome} />
              </span>
            </li>
          ))}
        </ul>

        <h4 className="t-body mt-6 font-semibold">Evidence</h4>
        <dl className="t-small mt-2 grid grid-cols-[1fr_auto] gap-x-4 gap-y-1">
          {evidence.map(([term, value]) => (
            <div key={term} className="contents">
              <dt className="text-muted">{term}</dt>
              <dd className="text-right">{value}</dd>
            </div>
          ))}
        </dl>

        <table className="t-small mt-6 w-full text-left">
          <caption className="t-body pb-2 text-left font-semibold">
            Distribution
          </caption>
          <thead>
            <tr data-taste-skill-ignore className="border-b border-ink/15">
              <th scope="col" className="py-1 font-medium text-muted">
                Split
              </th>
              <th
                scope="col"
                className="py-1 text-right font-medium text-muted"
              >
                Members
              </th>
            </tr>
          </thead>
          <tbody>
            {splits.map(([split, count]) => (
              <tr key={split} data-taste-skill-ignore className="border-b border-ink/15">
                <th scope="row" className="py-1 font-normal">
                  {split}
                </th>
                <td className="py-1 text-right">{count}</td>
              </tr>
            ))}
          </tbody>
        </table>

        <p className="t-small mt-5 text-muted">
          Example report. The numbers are illustrative.
        </p>
      </div>
    </article>
  );
}
