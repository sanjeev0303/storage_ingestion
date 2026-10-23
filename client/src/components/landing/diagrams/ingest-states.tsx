"use client";

import gsap from "gsap";
import { copy } from "../copy";
import {
  DiagramFigure,
  useDiagramRef,
  useDiagramTimeline,
} from "../diagram-kit";
import { Outcome } from "@/components/ui/status";
import { cn } from "@/lib/utils/cn";

const fieldTones = [
  "bg-ground text-ink",
  "bg-faint text-ink",
  "bg-[color-mix(in_srgb,var(--ink)_35%,var(--ground))] text-ink",
  "bg-ink text-ground",
];

const entry = copy.ingest.entry;

function StateLine({ index }: { index: number }) {
  if (index === 0) {
    return <Outcome outcome="not_assessed" label="Pending" />;
  }
  if (index === 1) {
    return (
      <div className="w-full">
        <span className="font-medium">Uploading</span>
        <div className="mt-2 h-1.5 w-full bg-faint">
          <div
            data-bar
            className="h-full origin-left bg-ink"
            style={{ transform: "scaleX(0.62)" }}
          />
        </div>
      </div>
    );
  }
  if (index === 2) {
    return <Outcome outcome="passed" label="AVAILABLE" />;
  }
  return (
    <div>
      <Outcome outcome="failed" label="Invalid" />
      <p className="mono-data mt-1">invalid_checksum</p>
      <p className="t-small mt-1">line 3 of the manifest, never uploaded</p>
    </div>
  );
}

function EntryCard({ index }: { index: number }) {
  return (
    <div
      data-ui
      data-card
      className="w-full rounded-md border border-[var(--edge)] bg-ground p-4 text-ink"
    >
      <p className="font-medium">{entry.path}</p>
      <p className="t-small mt-1">{entry.size}</p>
      <p className="mono-data mt-1">{entry.checksum}</p>
      <div data-state-line data-taste-skill-ignore className="t-small mt-4 border-t border-faint pt-3">
        <StateLine index={index} />
      </div>
    </div>
  );
}

export function IngestStates() {
  const ref = useDiagramRef();

  useDiagramTimeline(ref, (tl, root) => {
    const fields = gsap.utils.toArray<HTMLElement>("[data-field]", root);
    const token = root.querySelector<HTMLElement>("[data-hop]");
    const cards = gsap.utils.toArray<HTMLElement>("[data-card]", root);
    const lines = gsap.utils.toArray<HTMLElement>("[data-state-line]", root);
    const bar = root.querySelector<HTMLElement>("[data-bar]");

    // On phones the fields are stacked, so the token hops down the right edge instead.
    const spot = (field: HTMLElement) => {
      const strip = root.getBoundingClientRect();
      const box = field.getBoundingClientRect();
      const stacked = box.width > strip.width * 0.9;
      return {
        x: box.right - strip.left - 56,
        y: stacked ? box.top - strip.top : 0,
      };
    };

    tl.set(
      lines,
      { autoAlpha: 0, scale: 0.9, transformOrigin: "left center" },
      0,
    );
    fields.forEach((field, i) => {
      const at = i * 1.5;
      tl.to(
        token,
        {
          x: () => spot(field).x,
          y: () => spot(field).y,
          duration: 0.45,
          ease: "power2.inOut",
        },
        at,
      );
      tl.to(cards[i], { y: -6, scale: 1.02, duration: 0.2 }, at + 0.4);
      if (i === 1 && bar) {
        tl.fromTo(
          bar,
          { scaleX: 0 },
          { scaleX: 0.62, duration: 0.8, ease: "none" },
          at + 0.5,
        );
        tl.to(lines[i], { autoAlpha: 1, scale: 1, duration: 0.2 }, at + 0.5);
      } else {
        tl.to(
          lines[i],
          { autoAlpha: 1, scale: 1, duration: 0.25, ease: "back.out(2)" },
          at + 0.5,
        );
      }
      tl.to(cards[i], { y: 0, scale: 1, duration: 0.2 }, at + 1.25);
    });
  });

  return (
    <DiagramFigure label={copy.ingest.diagramLabel}>
      <div ref={ref} className="relative grid md:grid-cols-4">
        <span
          data-hop
          aria-hidden
          className="absolute top-4 left-4 z-10 size-5 border-2 border-ink bg-accent"
        />
        {copy.ingest.states.map((state, i) => (
          <div
            key={state}
            data-field
            className={cn(
              "flex min-h-[56svh] flex-col px-6 pt-14 pb-8 md:min-h-[70svh] md:px-8",
              i === 0 && "lg:pl-[max(88px,calc((100vw-var(--content-max))/2+88px))]",
              fieldTones[i],
            )}
          >
            <h3 className="t-lead font-semibold">{state}</h3>
            <p className="t-body mt-3 max-w-[30ch]">
              {copy.ingest.stateNotes[i]}
            </p>
            <div className="mt-auto pt-10">
              <EntryCard index={i} />
            </div>
          </div>
        ))}
      </div>
    </DiagramFigure>
  );
}
