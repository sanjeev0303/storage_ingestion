"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, useState, type KeyboardEvent, type Ref } from "react";
import { cn } from "@/lib/utils/cn";
import { copy } from "../copy";
import { Mark } from "../diagram-kit";

gsap.registerPlugin(useGSAP, ScrollTrigger);

const markFor = {
  Available: "passed",
  Processing: "partial",
  Quarantined: "not_assessed",
  Rejected: "failed",
} as const;

type Status = keyof typeof markFor;

// One row walks through the real life of an object while the board is on screen.
const CYCLE: Status[] = ["Quarantined", "Processing", "Available"];
const CYCLING_ROW = 4;

function StatusBlock({ status, flipRef }: { status: Status; flipRef?: Ref<HTMLSpanElement> }) {
  return (
    <span ref={flipRef} className="flex items-center justify-end gap-2 font-medium md:w-36">
      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden className="shrink-0">
        <Mark kind={markFor[status]} x={0} y={0} />
      </svg>
      {status}
    </span>
  );
}

export function CatalogBoard() {
  const [open, setOpen] = useState(0);
  const [cycleStep, setCycleStep] = useState(0);
  const list = useRef<HTMLUListElement>(null);
  const flip = useRef<HTMLSpanElement>(null);

  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        const tl = gsap.timeline({ paused: true, repeat: -1 });
        // Hold each status for a while, flip it out, swap the word, flip it back.
        tl.to(flip.current, { scaleY: 0, autoAlpha: 0, duration: 0.2, ease: "power2.in" }, 2.6)
          .call(() => setCycleStep((step) => (step + 1) % CYCLE.length))
          .to(flip.current, { scaleY: 1, autoAlpha: 1, duration: 0.25, ease: "power2.out" });

        ScrollTrigger.create({
          trigger: list.current,
          start: "top bottom",
          end: "bottom top",
          onToggle: (self) => (self.isActive ? tl.play() : tl.pause()),
        });
      });
    },
    { scope: list },
  );

  function moveFocus(event: KeyboardEvent<HTMLUListElement>) {
    const keys = ["ArrowDown", "ArrowUp", "Home", "End"];
    if (!keys.includes(event.key)) return;
    const buttons = Array.from(list.current?.querySelectorAll<HTMLButtonElement>("button[data-row]") ?? []);
    const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
    if (current === -1) return;
    event.preventDefault();
    let next = current;
    if (event.key === "ArrowDown") next = Math.min(current + 1, buttons.length - 1);
    if (event.key === "ArrowUp") next = Math.max(current - 1, 0);
    if (event.key === "Home") next = 0;
    if (event.key === "End") next = buttons.length - 1;
    buttons[next].focus();
  }

  return (
    <ul ref={list} onKeyDown={moveFocus} className="m-0 flex list-none flex-col gap-1.5 p-0">
      {copy.catalog.rows.map((row, index) => {
        const isOpen = open === index;
        const status: Status = index === CYCLING_ROW ? CYCLE[cycleStep] : row.status;
        const hasVersions = row.path === copy.catalog.versionsOf;

        return (
          <li key={row.path} className="bg-[color-mix(in_srgb,var(--ground)_8%,var(--ink))]">
            <button
              type="button"
              data-row
              aria-expanded={isOpen}
              aria-controls={`catalog-panel-${index}`}
              onClick={() => setOpen(isOpen ? -1 : index)}
              className="t-body grid min-h-16 w-full cursor-pointer grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 px-4 text-left transition-transform duration-150 ease-out outline-offset-[-2px] focus-visible:outline-accent active:scale-[0.99] md:min-h-[72px] md:grid-cols-[minmax(0,2.4fr)_5rem_6rem_9rem_auto] md:px-6"
            >
              <span className="font-medium">{row.path}</span>
              <span className="hidden tabular-nums md:block">
                {row.versions} {row.versions === 1 ? "version" : "versions"}
              </span>
              <span className="hidden tabular-nums md:block">{row.size}</span>
              <span className="mono-data hidden md:block">{row.sha}</span>
              <StatusBlock status={status} flipRef={index === CYCLING_ROW ? flip : undefined} />
            </button>

            <div
              id={`catalog-panel-${index}`}
              inert={!isOpen}
              className={cn(
                "grid transition-[grid-template-rows] duration-200 ease-out motion-reduce:transition-none",
                isOpen ? "grid-rows-[1fr]" : "grid-rows-[0fr]",
              )}
            >
              <div className="overflow-hidden">
                <div className="px-4 pt-1 pb-5 md:px-6">
                  <dl className="t-small mb-4 flex flex-wrap gap-x-8 gap-y-2 md:hidden">
                    <div>
                      <dt className="text-ground/80">Versions</dt>
                      <dd className="m-0">{row.versions}</dd>
                    </div>
                    <div>
                      <dt className="text-ground/80">Size</dt>
                      <dd className="m-0">{row.size}</dd>
                    </div>
                    <div>
                      <dt className="text-ground/80">SHA-256</dt>
                      <dd className="mono-data m-0">{row.sha}</dd>
                    </div>
                  </dl>

                  {hasVersions ? (
                    <ul className="m-0 flex list-none flex-col gap-3 p-0">
                      {copy.catalog.versionRows.map((version, versionIndex) => (
                        <li key={version.v} className="t-body flex flex-wrap items-center gap-x-4 gap-y-1">
                          <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden className="shrink-0">
                            <Mark kind={versionIndex === 2 ? "not_assessed" : "passed"} x={0} y={0} />
                          </svg>
                          <span className="w-24 font-semibold">{version.v}</span>
                          <span className="min-w-0 flex-1 basis-48">{version.note}</span>
                          <span className="mono-data">{version.sha}</span>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <p className="t-body m-0">
                      Version {row.versions} is current. Status: {status}.
                    </p>
                  )}
                </div>
              </div>
            </div>
          </li>
        );
      })}
    </ul>
  );
}
