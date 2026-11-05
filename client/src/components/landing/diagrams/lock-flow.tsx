"use client";

import { useId, useRef, useState } from "react";
import { copy } from "../copy";
import { DiagramFigure, Token } from "../diagram-kit";
import { Box, StepList, Switch, Wire, useTokenRun, type Step } from "../flow-kit";

const c = copy.lock;
type ToggleId = (typeof c.toggles)[number]["id"];

// The three questions sit side by side; a yes drops to "kept".
const Q = [
  { id: "hold", x: 184 },
  { id: "retention", x: 336 },
  { id: "lock", x: 488 },
] as const;
const W = 120;

export function LockFlow() {
  const ref = useRef<HTMLDivElement>(null);
  const legend = useId();
  const animate = useRef(false);
  const [on, setOn] = useState<Record<ToggleId, boolean>>({ hold: true, retention: false, lock: false });
  const first = Q.findIndex((q) => on[q.id]);
  const kept = first >= 0;

  const path: [number, number][] = [[84, 68]];
  for (let i = 0; i < Q.length; i++) {
    const cx = Q[i].x + W / 2;
    path.push([cx, 68]);
    if (i === first) {
      path.push([cx, 194]);
      break;
    }
  }
  if (!kept) path.push([700, 68], [700, 194]);
  useTokenRun(ref, path, JSON.stringify(on), animate);
  const [ex, ey] = path[path.length - 1];
  const steps: Step[] = [{ label: c.request, state: "on" }];
  Q.forEach((q, i) => {
    if (kept && i > first) return;
    steps.push({ label: c.toggles[i].question, note: i === first ? "yes" : "no", state: "on" });
  });
  if (kept) steps.push({ label: c.outcomes.kept, state: "end" });
  else steps.push({ label: c.outcomes.purged, state: "on" }, { label: c.outcomes.certificate, state: "end" });

  return (
    <div className="grid gap-10">
      <fieldset className="min-w-0 border-0 p-0" aria-labelledby={legend}>
        <legend id={legend} className="t-small mb-2 p-0 font-medium">
          {c.toggleLabel}
        </legend>
        <div className="flex flex-wrap gap-2">
          {c.toggles.map((t) => (
            <Switch
              key={t.id}
              label={t.name}
              checked={on[t.id]}
              onChange={(value) => {
                animate.current = true;
                setOn({ ...on, [t.id]: value });
              }}
            />
          ))}
        </div>
      </fieldset>

      <div ref={ref}>
        <StepList steps={steps} />
        <div className="hidden md:block">
          <DiagramFigure label={c.diagramLabel}>
            <svg viewBox="0 0 800 284" className="mx-auto block h-auto w-full max-w-[800px]" fill="none">
              <Wire
                points={[
                  [152, 68],
                  [184, 68],
                ]}
              />
              {Q.map((q, i) => {
                const cx = q.x + W / 2;
                const reached = first < 0 || i <= first;
                const next = i < Q.length - 1 ? Q[i + 1].x : 640;
                return (
                  <g key={q.id}>
                    <Wire
                      points={[
                        [cx, 96],
                        [cx, 212],
                      ]}
                      on={i === first}
                      label={{ x: cx + 28, y: 158, text: "yes" }}
                    />
                    <Wire
                      points={[
                        [q.x + W, 68],
                        [next, 68],
                      ]}
                      on={reached && i !== first && (first < 0 || i < first)}
                    />
                  </g>
                );
              })}
              <Wire
                points={[
                  [700, 96],
                  [700, 212],
                ]}
                on={!kept}
              />
              <Wire
                points={[
                  [184, 240],
                  [84, 240],
                  [84, 96],
                ]}
                on={kept}
                label={{ x: 120, y: 268, text: "later" }}
              />

              <Box x={16} y={40} w={136} h={56} title={c.request} />
              {Q.map((q, i) => (
                <Box key={q.id} x={q.x} y={40} w={W} h={56} title={c.toggles[i].question} state={first < 0 || i <= first ? "on" : "off"} />
              ))}
              <Box x={640} y={40} w={144} h={56} title={c.outcomes.purged} state={kept ? "off" : "on"} />
              <Box x={184} y={212} w={424} h={56} title={c.outcomes.kept} state={kept ? "end" : "off"} />
              <Box x={616} y={212} w={168} h={56} title={c.outcomes.certificate} state={kept ? "off" : "end"} />
              <Token x={ex - 8} y={ey - 8} />
            </svg>
          </DiagramFigure>
        </div>
      </div>
    </div>
  );
}
