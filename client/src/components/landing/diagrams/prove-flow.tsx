"use client";

import { useRef, useState } from "react";
import { Outcome } from "@/components/ui/status";
import { copy } from "../copy";
import { DiagramFigure, Token } from "../diagram-kit";
import { Box, StepList, Switch, Wire, useTokenRun } from "../flow-kit";

const c = copy.prove;
const ROW = { x: 16, w: 368, h: 42, gap: 58, top: 16 };
const rowY = (i: number) => ROW.top + i * ROW.gap;
const TAMPERED = 2; // event 3

export function ProveFlow() {
  const ref = useRef<HTMLDivElement>(null);
  const animate = useRef(false);
  const [tampered, setTampered] = useState(false);
  const from = tampered ? TAMPERED : 0;
  const path: [number, number][] = [];
  for (let i = from; i < c.events.length; i++) path.push([40, rowY(i) + ROW.h / 2]);
  path.push([424, rowY(4) + ROW.h / 2], [424, 156], [580, 156], [580, 110]);
  useTokenRun(ref, path, String(tampered), animate);
  const [ex, ey] = path[path.length - 1];

  return (
    <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
      <div ref={ref} className="lg:col-span-8">
        <StepList
          steps={[
            ...c.events.map((e, i) => ({
              label: `${i + 1}. ${tampered && i === TAMPERED ? "asset.delete" : e.action}`,
              note: e.who,
              state: tampered && i === TAMPERED ? ("end" as const) : tampered && i > TAMPERED ? ("off" as const) : ("on" as const),
            })),
            { label: c.anchor, state: tampered ? ("off" as const) : ("end" as const) },
          ]}
        />
        <div className="hidden md:block">
          <DiagramFigure label={c.diagramLabel}>
            <svg viewBox="0 0 712 312" className="block h-auto w-full max-w-[712px]" fill="none">
              {c.events.slice(0, -1).map((_, i) => (
                <Wire
                  key={i}
                  points={[
                    [40, rowY(i) + ROW.h],
                    [40, rowY(i + 1)],
                  ]}
                  on={!tampered || i < TAMPERED - 1}
                />
              ))}
              <Wire
                points={[
                  [ROW.x + ROW.w, rowY(4) + ROW.h / 2],
                  [424, rowY(4) + ROW.h / 2],
                  [424, 156],
                  [456, 156],
                ]}
                on={!tampered}
              />

              {c.events.map((e, i) => {
                const broken = tampered && i > TAMPERED;
                const changed = tampered && i === TAMPERED;
                const ink = changed ? { fill: "var(--on-accent)" } : undefined;
                const y = rowY(i);
                return (
                  <g key={i}>
                    <rect
                      x={ROW.x}
                      y={y}
                      width={ROW.w}
                      height={ROW.h}
                      fill={changed ? "var(--accent)" : "var(--box)"}
                      stroke={changed ? "var(--on-accent)" : "currentColor"}
                      strokeWidth={broken ? 1.5 : 2}
                      strokeDasharray={broken ? "5 4" : undefined}
                    />
                    <text x={ROW.x + 44} y={y + 27} fontSize="14" fontWeight="600" style={ink}>
                      {i + 1}
                    </text>
                    <text x={ROW.x + 72} y={y + 27} fontSize="13" className="mono" style={ink}>
                      {changed ? "asset.delete" : e.action}
                    </text>
                    <text x={ROW.x + ROW.w - 12} y={y + 27} fontSize="13" textAnchor="end" style={ink}>
                      {e.who}
                    </text>
                  </g>
                );
              })}

              <Box
                x={456}
                y={124}
                w={248}
                h={64}
                title={c.anchor.split(",")[0]}
                sub={tampered ? "does not match: broken" : "matches: verified"}
                state={tampered ? "off" : "end"}
              />
              <Token x={ex - 8} y={ey - 8} />
            </svg>
          </DiagramFigure>
        </div>
      </div>

      <div className="lg:col-span-4">
        <Switch
          label={c.tamperLabel}
          checked={tampered}
          onChange={(value) => {
            animate.current = true;
            setTampered(value);
          }}
        />
        <div data-ui aria-live="polite" className="mt-6 bg-faint p-5 text-ink">
          <Outcome outcome={tampered ? "failed" : "passed"} label={tampered ? "Verification failed" : "Verification passed"} />
          <p className="t-body mt-2">{tampered ? c.broken : c.intact}</p>
        </div>
      </div>
    </div>
  );
}
