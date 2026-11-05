"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, useState } from "react";
import { DiagramFigure, Token } from "@/components/landing/diagram-kit";
import { Box, Choices, Wire, useChoice, useTokenRun } from "@/components/landing/flow-kit";
import { map } from "./home-copy";

gsap.registerPlugin(useGSAP, ScrollTrigger);

type StageId = (typeof map.stages)[number]["id"];

// The main path runs left to right. Second copy, governance and evidence are
// rails the full width of the path: they run beside every stage, as the copy
// says. Box origins on a 4px grid; the drawing never renders below 1:1.
const W = 136;
const H = 56;
const GAP = 32;
const ROW = 100;
const MAIN: Record<string, number> = { arrive: 16, gate: 184, catalog: 352, snapshot: 520, report: 688, use: 856 };
const RAIL = {
  copy: { x: 16, y: 16, w: 976 },
  govern: { x: 16, y: 188, w: 976 },
  evidence: { x: 16, y: 256, w: 976 },
} as const;

function place(id: StageId) {
  if (id in MAIN) return { x: MAIN[id], y: ROW, w: W };
  return RAIL[id as keyof typeof RAIL];
}

// Where the token rests for a stage: on the top edge of its box, a rail's at
// the middle of the main path.
function rest(id: StageId): [number, number] {
  const p = place(id);
  return id in MAIN ? [p.x + W / 2, p.y] : [520 + W / 2, p.y];
}

export function PlatformMap() {
  const ref = useRef<HTMLDivElement>(null);
  const choice = useChoice<StageId>("gate");
  // The stage chosen before this one, so the token can walk from there.
  const [from, setFrom] = useState<StageId>("gate");
  // Once someone has chosen a stage, the first-view walk no longer runs.
  const touched = useRef(false);
  const stage = map.stages.find((s) => s.id === choice.value) ?? map.stages[1];
  const [ex, ey] = rest(stage.id);

  // A choice moves the token from the last stage to this one, along the wire row.
  const [fx] = rest(from);
  const mid = ROW + H / 2;
  useTokenRun(ref, [rest(from), [fx, mid], [ex, mid], [ex, ey]], choice.value, choice.animate);

  // The first time the map scrolls into view the token walks the whole path,
  // then settles on the chosen stage.
  useGSAP(
    () => {
      const token = ref.current?.querySelector("[data-token]");
      if (!token) return;
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        const tl = gsap.timeline({ paused: true, defaults: { duration: 0.32, ease: "power2.inOut" } });
        const [rx, ry] = rest("gate");
        tl.set(token, { x: MAIN.arrive + W / 2 - rx, y: 0 });
        for (const x of Object.values(MAIN).slice(1)) tl.to(token, { x: x + W / 2 - rx });
        tl.to(token, { y: RAIL.govern.y - ry + 6 }).to(token, { y: 0, x: 0 }, "+=0.2");
        ScrollTrigger.create({
          trigger: ref.current,
          start: "top 70%",
          once: true,
          onEnter: () => {
            if (!touched.current) tl.play();
          },
        });
      });
      return () => mm.revert();
    },
    { scope: ref },
  );

  return (
    <div ref={ref} className="grid gap-8">
      <Choices
        legend={map.pick}
        options={map.stages.map((s) => ({ id: s.id, label: s.name }))}
        value={choice.value}
        onChoose={(id) => {
          touched.current = true;
          setFrom(choice.value);
          choice.choose(id);
        }}
        onKeyDown={choice.onKeyDown}
        onPointerDown={choice.onPointerDown}
      />

      <div className="grid gap-8 xl:grid-cols-[minmax(0,1000px)_minmax(0,1fr)] xl:items-start">
        <div data-ui aria-live="polite" className="label-cut bg-[var(--h-tint-1)] p-6 text-[var(--h-ink)] [--cut:28px] xl:order-2 xl:p-8">
          <h3 className="h-lead">{stage.name}</h3>
          <p className="h-body mt-3">
            <span className="font-medium">Fixes: </span>
            {stage.fixes}
          </p>
          <ul className="mt-5 grid gap-2">
            {stage.features.map((f) => (
              <li key={f} className="h-body flex gap-3">
                <span aria-hidden className="label-cut mt-[0.45em] size-2.5 shrink-0 bg-[#ff7a1f] [--cut:3px]" />
                {f}
              </li>
            ))}
          </ul>
          <a href={stage.chapter} className="h-small h-link mt-6 inline-block font-medium">
            {map.more}
          </a>
        </div>

        <div className="hidden min-w-0 md:block xl:order-1">
          <DiagramFigure label={map.diagramLabel}>
            <svg viewBox="0 0 1008 328" className="block h-auto w-full max-w-[1008px]" fill="none">
              {(["arrive", "gate", "catalog", "snapshot", "report"] as const).map((id) => (
                <Wire key={id} points={[[MAIN[id] + W, ROW + H / 2], [MAIN[id] + W + GAP, ROW + H / 2]]} />
              ))}
              {map.stages.map((s) => {
                const p = place(s.id);
                return <Box key={s.id} x={p.x} y={p.y} w={p.w} h={H} title={s.name} sub={s.sub} state={s.id === stage.id ? "end" : "on"} />;
              })}
              <Token x={ex - 8} y={ey - 8} />
            </svg>
          </DiagramFigure>
        </div>
      </div>
    </div>
  );
}
