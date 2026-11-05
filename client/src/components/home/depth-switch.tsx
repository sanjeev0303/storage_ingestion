"use client";

import { useId, useState } from "react";
import { depth } from "./home-copy";

type ModeId = (typeof depth.modes)[number]["id"];

// One control, blown up: the depth a check reads. Choosing a depth changes the
// field (nothing read, a sample, everything), the number in the heading, and
// which of the 48 members are marked read.
const FIELD: Record<ModeId, string> = {
  metadata: "field-b [--chip-on:var(--h-b)]",
  sampled: "field-a [--chip-on:var(--h-orange)]",
  full: "field-night [--chip-on:var(--h-night)]",
};

// Sampled reads every sixth member, as a deterministic sample does.
function isRead(mode: ModeId, i: number) {
  return mode === "full" || (mode === "sampled" && i % 6 === 0);
}

export function DepthSwitch() {
  const name = useId();
  const [id, setId] = useState<ModeId>("sampled");
  const mode = depth.modes.find((m) => m.id === id) ?? depth.modes[1];

  return (
    <section
      id="depth"
      aria-labelledby="depth-title"
      className={`relative overflow-hidden pb-20 transition-colors duration-200 lg:min-h-[92svh] lg:pb-0 ${FIELD[id]}`}
    >
      <div className="mx-auto max-w-[1440px] px-4 pt-20 sm:px-10 lg:pt-28">
        <h2 id="depth-title" data-reveal className="h-head" aria-live="polite">
          {depth.heading(mode.read, depth.total)}
        </h2>
        <p data-reveal className="h-body mt-6 max-w-[44ch]">
          {mode.text}
        </p>

        <ol aria-label={`${mode.read} of ${depth.total} members read`} className="mt-12 grid max-w-[30rem] grid-cols-12 gap-[6px]">
          {Array.from({ length: depth.total }, (_, i) => (
            <li key={i} aria-hidden className="aspect-square">
              <svg viewBox="0 0 16 16" className="size-full">
                <rect
                  x="1.5"
                  y="1.5"
                  width="13"
                  height="13"
                  fill={isRead(id, i) ? "currentColor" : "none"}
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeDasharray={isRead(id, i) ? undefined : "2.5 2"}
                />
              </svg>
            </li>
          ))}
        </ol>
      </div>

      <fieldset
        data-ui
        className="relative mx-4 mt-12 min-w-0 border-0 p-0 text-[clamp(1.125rem,5vw,1.5rem)] sm:mx-10 lg:absolute lg:right-[-4vw] lg:bottom-[10vh] lg:m-0 lg:text-[clamp(1.25rem,3vw,3rem)]"
      >
        <legend className="h-small mb-4 p-0 font-medium">{depth.legend}</legend>
        <div className="flex flex-col gap-[6px] lg:flex-row lg:pr-[3em]">
          {depth.modes.map((m) => (
            <label
              key={m.id}
              className="label-cut flex min-h-[2.6em] cursor-pointer items-center bg-[color-mix(in_srgb,currentColor_14%,transparent)] px-[0.9em] font-medium whitespace-nowrap transition-colors duration-150 [--cut:0.5em] has-[:checked]:bg-current has-[:focus-visible]:shadow-[inset_0_0_0_3px_currentColor]"
            >
              <input type="radio" name={name} value={m.id} checked={m.id === id} onChange={() => setId(m.id)} className="sr-only" />
              <span className={m.id === id ? "text-[var(--chip-on)]" : undefined}>{m.label}</span>
            </label>
          ))}
        </div>
        <p className="h-small mt-4 tabular-nums">
          Content read: {mode.read} of {depth.total} members
        </p>
      </fieldset>
    </section>
  );
}
