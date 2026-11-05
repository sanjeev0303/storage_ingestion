"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { useId, useRef, useState, type KeyboardEvent } from "react";
import { OutcomeMark } from "@/components/ui/status";
import { slip, type Outcome } from "./home-copy";
import { Barcode } from "./motif";

gsap.registerPlugin(useGSAP);

type SampleId = (typeof slip.samples)[number]["id"];

function Row({ name, outcome, word, children }: { name: string; outcome: Outcome; word: string; children?: React.ReactNode }) {
  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-1 py-2.5">
      <span className="h-small font-medium">{name}</span>
      <span className="h-small inline-flex items-center gap-2 font-medium">
        <OutcomeMark outcome={outcome} />
        {word}
      </span>
      {children && <div className="col-span-2">{children}</div>}
    </li>
  );
}

// The live thing on the page: pick an upload and read the label it gets.
export function AccessionSlip() {
  const root = useRef<HTMLDivElement>(null);
  const name = useId();
  const [id, setId] = useState<SampleId>("model");
  const animate = useRef(false);
  const arrowKey = useRef(false);
  const s = slip.samples.find((x) => x.id === id) ?? slip.samples[0];
  const rejected = s.released === "rejected";
  const content: Outcome = rejected ? "failed" : s.released === "flagged" ? "warning" : "passed";

  useGSAP(
    () => {
      if (!animate.current) return;
      animate.current = false;
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        gsap.fromTo("[data-print]", { y: 10, opacity: 0.2 }, { y: 0, opacity: 1, duration: 0.32, ease: "power3.out", stagger: 0.04 });
        gsap.fromTo("[data-stamp]", { scale: 1.12, rotate: -4 }, { scale: 1, rotate: -2, duration: 0.3, ease: "power3.out", delay: 0.12 });
      });
      return () => mm.revert();
    },
    { scope: root, dependencies: [id] },
  );

  function choose(next: SampleId) {
    animate.current = !arrowKey.current;
    arrowKey.current = false;
    setId(next);
  }

  return (
    <div ref={root} className="w-full max-w-[620px]">
      <fieldset
        className="m-0 min-w-0 border-0 p-0 [--chip-on:var(--h-orange)]"
        onKeyDown={(e: KeyboardEvent) => {
          if (e.key.startsWith("Arrow")) arrowKey.current = true;
        }}
        onPointerDown={() => (arrowKey.current = false)}
      >
        <legend className="h-small mb-3 p-0 font-medium">{slip.pick}</legend>
        <div className="flex flex-wrap gap-2">
          {slip.samples.map((x) => (
            <label key={x.id} className="h-chip h-small">
              <input type="radio" name={name} value={x.id} checked={x.id === id} onChange={() => choose(x.id)} className="sr-only" />
              <span translate="no">{x.file}</span>
            </label>
          ))}
        </div>
      </fieldset>

      <div data-ui className="label-cut relative mt-4 bg-[#ffffff] px-6 pt-7 pb-10 text-[#050f16] [--cut:44px] sm:px-8">
        <p className="h-small font-medium">{slip.title}</p>
        <div data-print className="mt-3 flex flex-wrap items-end justify-between gap-x-6 gap-y-2">
          <p className="h-head leading-none" translate="no">
            {s.accession}
          </p>
          <p className="h-small text-right">
            <span translate="no" className="block font-medium">
              {s.file}
            </span>
            {s.size}
          </p>
        </div>
        <div data-print className="mt-5 flex items-center gap-4">
          <Barcode value={s.sha + s.accession} className="h-10 w-40 shrink-0" />
          <span className="mono-data h-small" translate="no">
            sha256:{s.sha}
          </span>
        </div>

        <ul data-print aria-live="polite" className="mt-3 divide-y-2 divide-[#050f16]/12">
          <Row name={slip.rows.sum} outcome="passed" word="Passed" />
          <Row name={slip.rows.content} outcome={content} word={rejected ? "Failed" : s.released === "flagged" ? "Finding" : "Passed"}>
            <p className="h-small">
              {s.code && (
                <>
                  <span className="mono-data" translate="no">
                    {s.code}
                  </span>{" "}
                </>
              )}
              {s.finding}
            </p>
          </Row>
          <Row name={slip.rows.scan} outcome={rejected ? "not_assessed" : "passed"} word={rejected ? "Not reached" : "Clean"} />
          <Row name={slip.rows.label} outcome="not_assessed" word="Not tested" />
        </ul>

        <p
          data-stamp
          className={`absolute right-6 bottom-4 -rotate-2 border-[3px] border-current px-3 py-1 h-small font-medium sm:right-8 ${
            rejected ? "bg-[#050f16] text-[#ff7a1f]" : ""
          }`}
        >
          {slip.released[s.released]}
        </p>
      </div>
      <p className="h-small mt-4 max-w-[52ch]">{slip.notTested}</p>
    </div>
  );
}
