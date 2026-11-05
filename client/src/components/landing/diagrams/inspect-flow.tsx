"use client";

import { useRef } from "react";
import { Outcome } from "@/components/ui/status";
import { copy } from "../copy";
import { DiagramFigure, Token } from "../diagram-kit";
import { Box, Choices, StepList, Wire, useChoice, useTokenRun } from "../flow-kit";

const c = copy.inspect;
type SampleId = (typeof c.samples)[number]["id"];

// Box centres the token passes through.
const AT = { upload: [72, 68], sum: [224, 68], inspect: [400, 68], scan: [588, 68], rejected: [400, 182], out: [588, 182] } as const;

export function InspectFlow() {
  const ref = useRef<HTMLDivElement>(null);
  const choice = useChoice<SampleId>("model");
  const sample = c.samples.find((s) => s.id === choice.value) ?? c.samples[0];
  const rejected = sample.outcome === "rejected";
  const path = rejected ? [AT.upload, AT.sum, AT.inspect, AT.rejected] : [AT.upload, AT.sum, AT.inspect, AT.scan, AT.out];
  useTokenRun(ref, path, choice.value, choice.animate);
  const [ex, ey] = path[path.length - 1];

  return (
    <div className="grid gap-8 lg:grid-cols-12 lg:items-start">
      <div className="lg:col-span-4">
        <Choices
          legend={c.pickLabel}
          options={c.samples.map((s) => ({ id: s.id, label: s.file }))}
          value={choice.value}
          onChoose={choice.choose}
          onKeyDown={choice.onKeyDown}
          onPointerDown={choice.onPointerDown}
        />
        <div data-ui aria-live="polite" className="mt-6 bg-faint p-5 text-ink">
          <div className="flex items-center justify-between gap-3">
            <span className="t-body font-semibold">{sample.check}</span>
            <Outcome outcome={rejected ? "failed" : sample.outcome === "flagged" ? "warning" : "passed"} />
          </div>
          <p translate="no" className="mono-data t-small mt-2">{sample.code}</p>
          <p className="t-body mt-2">{sample.finding}</p>
        </div>
      </div>

      <div ref={ref} className="lg:col-span-8">
        <StepList
          steps={[
            { label: c.stages.upload, state: "on" },
            { label: c.stages.sum, state: "on" },
            { label: c.stages.inspect, note: sample.check, state: "on" },
            { label: c.stages.scan, state: rejected ? "off" : "on" },
            {
              label: rejected ? c.outcomes.rejected : sample.outcome === "flagged" ? c.outcomes.flagged : c.outcomes.published,
              state: "end",
            },
          ]}
        />
        <div className="hidden md:block">
          <DiagramFigure label={c.diagramLabel}>
            <svg viewBox="0 0 680 280" className="block h-auto w-full max-w-[680px]" fill="none">
              <Wire
                points={[
                  [128, 68],
                  [168, 68],
                ]}
              />
              <Wire
                points={[
                  [280, 68],
                  [320, 68],
                ]}
              />
              <Wire
                points={[
                  [480, 68],
                  [520, 68],
                ]}
                on={!rejected}
              />
              <Wire
                points={[
                  [400, 96],
                  [400, 200],
                ]}
                on={rejected}
                label={{ x: 446, y: 152, text: "reject" }}
              />
              <Wire
                points={[
                  [588, 96],
                  [588, 200],
                ]}
                on={!rejected}
                label={{ x: 636, y: 152, text: "pass" }}
              />

              <Box x={16} y={40} w={112} h={56} title={c.stages.upload} />
              <Box x={168} y={40} w={112} h={56} title={c.stages.sum} />
              <Box x={320} y={40} w={160} h={56} title={c.stages.inspect} sub={sample.check} />
              <Box x={520} y={40} w={136} h={56} title={c.stages.scan} state={rejected ? "off" : "on"} />
              <Box x={320} y={200} w={160} h={56} title={c.outcomes.rejected} state={rejected ? "end" : "off"} />
              <Box
                x={500}
                y={200}
                w={176}
                h={56}
                title={sample.outcome === "flagged" ? c.outcomes.flagged : c.outcomes.published}
                state={rejected ? "off" : "end"}
              />
              <Token x={ex - 8} y={ey - 8} />
            </svg>
          </DiagramFigure>
        </div>
      </div>
    </div>
  );
}
