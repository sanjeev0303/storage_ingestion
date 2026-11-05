"use client";

import { useRef } from "react";
import { copy } from "../copy";
import { DiagramFigure, Token } from "../diagram-kit";
import { Box, Choices, StepList, Wire, useChoice, useTokenRun } from "../flow-kit";

const c = copy.stream;
type FrameworkId = (typeof c.frameworks)[number]["id"];

const ROWS = [40, 120, 200] as const; // three shards, three workers: an example, not a limit
const PATH = [
  [76, 148],
  [236, 148],
  [416, 148],
  [524, 148],
] as const;

export function StreamFlow() {
  const ref = useRef<HTMLDivElement>(null);
  const choice = useChoice<FrameworkId>("torch");
  const framework = c.frameworks.find((f) => f.id === choice.value) ?? c.frameworks[0];
  useTokenRun(ref, PATH, choice.value, choice.animate);

  return (
    <div className="grid gap-10 lg:grid-cols-12 lg:items-center">
      <div ref={ref} className="min-w-0 lg:col-span-8">
        <StepList
          steps={[
            { label: c.stages.snapshot, note: "pinned", state: "on" },
            { label: c.stages.shards, note: c.shardNote, state: "on" },
            { label: c.stages.workers, note: c.stages.check, state: "on" },
            { label: framework.name, state: "end" },
          ]}
        />
        <div className="hidden md:block">
          <DiagramFigure label={c.diagramLabel}>
            <svg viewBox="0 0 720 280" className="block h-auto w-full max-w-[720px]" fill="none">
              {ROWS.map((y, i) => (
                <g key={y}>
                  <Wire
                    points={[
                      [136, 132 + i * 16],
                      [156, 132 + i * 16],
                      [156, y + 28],
                      [176, y + 28],
                    ]}
                  />
                  <Wire
                    points={[
                      [296, y + 28],
                      [336, y + 28],
                    ]}
                  />
                  <Wire
                    points={[
                      [496, y + 28],
                      [516, y + 28],
                      [516, 132 + i * 16],
                      [536, 132 + i * 16],
                    ]}
                  />
                </g>
              ))}
              <Box x={16} y={120} w={120} h={56} title={c.stages.snapshot} sub="pinned" />
              {ROWS.map((y, i) => (
                <Box key={`s${y}`} x={176} y={y} w={120} h={56} title={`Shard ${i + 1}`} />
              ))}
              {ROWS.map((y, i) => (
                <Box key={`w${y}`} x={336} y={y} w={160} h={56} title={`Worker ${i + 1}`} sub={c.stages.check} />
              ))}
              <Box x={536} y={120} w={168} h={56} title={framework.name} state="end" />
              <Token x={PATH[3][0] - 8} y={PATH[3][1] - 8} />
            </svg>
          </DiagramFigure>
        </div>
        <p className="t-small mt-3 hidden md:block">Shards are {c.shardNote}. Three are drawn; a snapshot has as many as its size needs.</p>
      </div>

      <div className="min-w-0 lg:col-span-4">
        <Choices
          legend={c.pickLabel}
          options={c.frameworks.map((f) => ({ id: f.id, label: f.name }))}
          value={choice.value}
          onChoose={choice.choose}
          onKeyDown={choice.onKeyDown}
          onPointerDown={choice.onPointerDown}
        />
        <pre translate="no" data-ui aria-live="polite" className="mt-6 overflow-x-auto bg-faint p-5 text-ink">
          <code className="mono-data t-small whitespace-pre-wrap [overflow-wrap:anywhere]">{framework.code}</code>
        </pre>
        <p className="t-body mt-4">{c.bench}</p>
      </div>
    </div>
  );
}
