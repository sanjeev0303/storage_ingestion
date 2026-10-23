"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { useRef } from "react";
import { copy } from "../copy";
import { Mark, usePrefersReducedMotion } from "../diagram-kit";

gsap.registerPlugin(useGSAP);

const { statements, total, outcomes } = copy.validate;
const outcomeKinds = ["not_assessed", "partial", "passed"] as const;

// Which of the 48 members a mode reads. Sampled takes every 6th, so the
// picture is the same every time.
export function readCells(mode: number) {
  const count = statements[mode].read;
  if (count === total)
    return Array.from({ length: total }, (_, index) => index);
  const step = Math.floor(total / Math.max(count, 1));
  return Array.from({ length: count }, (_, index) => index * step);
}

export function ValidateGrid({ mode }: { mode: number }) {
  const grid = useRef<HTMLDivElement>(null);
  const reduced = usePrefersReducedMotion();
  const read = new Set(readCells(mode));
  const statement = statements[mode];

  useGSAP(
    () => {
      const fills = gsap.utils.toArray<HTMLElement>(
        "[data-fill]",
        grid.current,
      );
      const on = { opacity: 1, scale: 1 };
      const off = { opacity: 0, scale: 0.5 };
      if (reduced) {
        fills.forEach((fill, index) =>
          gsap.set(fill, read.has(index) ? on : off),
        );
        return;
      }
      // Lit cells arrive in order; cells that go dark leave together.
      const lighting = fills.filter((_, index) => read.has(index));
      const dimming = fills.filter((_, index) => !read.has(index));
      if (dimming.length)
        gsap.to(dimming, {
          ...off,
          duration: 0.15,
          ease: "power2.out",
          overwrite: true,
        });
      if (lighting.length)
        gsap.to(lighting, {
          ...on,
          duration: 0.2,
          ease: "power2.out",
          stagger: 0.02,
          overwrite: true,
        });
    },
    { scope: grid, dependencies: [mode, reduced] },
  );

  return (
    <div data-ui className="w-full">
      <div
        ref={grid}
        role="img"
        aria-label={copy.validate.diagramLabel}
        className="mx-auto grid max-w-[17.5rem] grid-cols-8 gap-1 lg:max-w-[394px] lg:gap-1.5"
      >
        {Array.from({ length: total }, (_, index) => (
          <span
            key={index}
            className="relative aspect-square border border-dashed border-current"
          >
            <span data-fill className="absolute -inset-px bg-ink opacity-0" />
          </span>
        ))}
      </div>

      <div
        aria-live="polite"
        className="mx-auto mt-4 max-w-[17.5rem] lg:mt-6 lg:max-w-[394px]"
      >
        <p className="t-lead m-0 font-semibold tabular-nums">
          Content read: {read.size} of {total}
        </p>
        <p className="t-body m-0 mt-1 flex flex-wrap items-center gap-x-6 gap-y-1">
          <span>Coverage: {statement.coverage}</span>
          <span className="flex items-center gap-2 font-semibold">
            <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden>
              <Mark kind={outcomeKinds[mode]} x={0} y={0} />
            </svg>
            {outcomes[mode]}
          </span>
        </p>
      </div>
    </div>
  );
}
