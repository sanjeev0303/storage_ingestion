"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { useId, useRef, useState, type KeyboardEvent } from "react";
import { Outcome, type Outcome as OutcomeKind } from "@/components/ui/status";
import { copy } from "../copy";

gsap.registerPlugin(useGSAP);

type Row = { check: string; outcome: OutcomeKind; note?: string };

// Example values for the report fragment. They are labelled as an example in its title.
const ROWS: Row[] = [
  { check: "Sizes match the manifest", outcome: "passed", note: "1,204 of 1,204 objects" },
  { check: "Checksums recorded and verified", outcome: "passed" },
  { check: "Malware scan evidence", outcome: "passed" },
  { check: "Duplicate content", outcome: "warning", note: "3 objects share a checksum" },
  { check: "Required labels", outcome: "failed", note: "41 objects have no label" },
];

const MODES = [
  { id: "metadata", label: "Metadata only", outcome: "not_assessed", note: "Content was never read", examined: "0 of 1,204" },
  { id: "sampled", label: "Sampled", outcome: "partial", note: "Read 100 of 1,204 objects", examined: "100 of 1,204" },
  { id: "full", label: "Full integrity", outcome: "passed", note: "Read 1,204 of 1,204 objects", examined: "1,204 of 1,204" },
] as const satisfies readonly { id: string; label: string; outcome: OutcomeKind; note: string; examined: string }[];

type ModeId = (typeof MODES)[number]["id"];

export function HeroReport() {
  const root = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const groupName = useId();
  const [modeId, setModeId] = useState<ModeId>("metadata");
  const mode = MODES.find((m) => m.id === modeId) ?? MODES[0];

  // Arrow keys change the mode without animation; only a pointer choice animates.
  const usedArrowKey = useRef(false);
  const animateNextChange = useRef(false);

  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add(
        "(prefers-reduced-motion: no-preference)",
        () => {
          gsap.from("[data-row]", { autoAlpha: 0, y: 10, duration: 0.4, delay: 0.5, stagger: 0.09, ease: "power2.out" });
        },
        root.current ?? undefined,
      );
    },
    { scope: root },
  );

  useGSAP(
    () => {
      if (!animateNextChange.current) return;
      animateNextChange.current = false;
      const mm = gsap.matchMedia();
      mm.add(
        "(prefers-reduced-motion: no-preference)",
        () => {
          gsap.fromTo(
            "[data-swap-mark]",
            { scale: 0.9, transformOrigin: "left center" },
            { scale: 1, duration: 0.2, ease: "power2.out" },
          );
          gsap.fromTo(
            "[data-swap-text]",
            { autoAlpha: 0 },
            { autoAlpha: 1, duration: 0.2, ease: "power2.out" },
          );
        },
        root.current ?? undefined,
      );
    },
    { scope: root, dependencies: [modeId] },
  );

  function choose(id: ModeId) {
    animateNextChange.current = !usedArrowKey.current;
    usedArrowKey.current = false;
    setModeId(id);
  }

  function noteKey(event: KeyboardEvent) {
    if (event.key.startsWith("Arrow")) usedArrowKey.current = true;
  }

  return (
    <div ref={root} data-ui role="group" aria-labelledby={titleId} className="bg-faint p-5 text-ink sm:p-7">
      <p id={titleId} className="t-body font-semibold">
        {copy.hero.fragmentTitle}
      </p>

      <fieldset className="mt-5 min-w-0 border-0 p-0" onKeyDown={noteKey} onPointerDown={() => (usedArrowKey.current = false)}>
        <legend className="t-small mb-2 p-0 font-medium">{copy.hero.modeLabel}</legend>
        <div className="grid grid-cols-3 gap-2">
          {MODES.map((m) => (
            <label
              key={m.id}
              className="t-small flex min-h-11 cursor-pointer items-center justify-center border border-ink px-2 text-center font-medium has-[:checked]:bg-ink has-[:checked]:text-ground has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-offset-2 has-[:focus-visible]:outline-ink"
            >
              <input
                type="radio"
                name={groupName}
                value={m.id}
                checked={m.id === modeId}
                onChange={() => choose(m.id)}
                className="sr-only"
              />
              {m.label}
            </label>
          ))}
        </div>
      </fieldset>

      <ul data-taste-skill-ignore className="mt-5 divide-y divide-ink/15">
        {ROWS.map((row) => (
          <li key={row.check} data-row className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 py-3">
            <span className="t-body font-medium">{row.check}</span>
            <Outcome outcome={row.outcome} />
            {row.note && <span className="t-small col-span-2 text-muted">{row.note}</span>}
          </li>
        ))}
        <li data-row data-integrity className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 py-3">
          <span className="t-body font-medium">Content integrity</span>
          <span data-swap-mark className="inline-flex">
            <Outcome outcome={mode.outcome} />
          </span>
          <span data-swap-text className="t-small col-span-2 text-muted">
            {mode.note}
          </span>
        </li>
      </ul>

      <div aria-live="polite" aria-atomic="true" className="mt-3 bg-ground p-4">
        <p className="t-body font-semibold">
          Verdict: Failed (1 error, 1 warning) from the profile&apos;s thresholds. There is no score.
        </p>
        <p className="t-small mt-1">
          Content examined: <span data-swap-text className="tabular-nums">{mode.examined}</span>
        </p>
      </div>
    </div>
  );
}
