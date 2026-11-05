"use client";

import { useRef } from "react";
import { copy } from "../copy";
import { DiagramFigure, Mark, Token } from "../diagram-kit";
import { Box, Choices, StepList, Wire, useChoice, useTokenRun, type Step } from "../flow-kit";

const c = copy.copies;
type ScenarioId = (typeof c.scenarios)[number]["id"];

const PATHS: Record<ScenarioId, readonly (readonly [number, number])[]> = {
  replicate: [
    [86, 68],
    [301, 68],
    [301, 56],
    [608, 56],
    [608, 30],
  ],
  repair: [
    [301, 232],
    [301, 84],
    [560, 84],
    [301, 84],
    [301, 30],
  ],
  failover: [
    [560, 232],
    [560, 150],
    [350, 150],
    [350, 110],
    [350, 150],
    [656, 150],
    [656, 30],
  ],
};

const STEPS: Record<ScenarioId, Step[]> = {
  replicate: [
    { label: c.nodes.writer, state: "on" },
    { label: c.nodes.primary, state: "on" },
    { label: c.nodes.replica, note: "copied and verified", state: "end" },
  ],
  repair: [
    { label: c.nodes.scrub, note: "re-reads the primary", state: "on" },
    { label: c.nodes.replica, note: "its copy checked first", state: "on" },
    { label: c.nodes.primary, note: "chunk rewritten", state: "end" },
  ],
  failover: [
    { label: c.nodes.reader, state: "on" },
    { label: c.nodes.primary, note: "not answering", state: "off" },
    { label: c.nodes.replica, note: "serves the read", state: "end" },
  ],
};

export function CopiesFlow() {
  const ref = useRef<HTMLDivElement>(null);
  const choice = useChoice<ScenarioId>("replicate");
  const id = choice.value;
  const scenario = c.scenarios.find((s) => s.id === id) ?? c.scenarios[0];
  const path = PATHS[id];
  useTokenRun(ref, path, id, choice.animate);
  const [ex, ey] = path[path.length - 1];

  return (
    <div className="grid gap-10 lg:grid-cols-12 lg:items-center">
      <div className="lg:col-span-4">
        <Choices
          legend={c.pickLabel}
          options={c.scenarios.map((s) => ({ id: s.id, label: s.name }))}
          value={id}
          onChoose={choice.choose}
          onKeyDown={choice.onKeyDown}
          onPointerDown={choice.onPointerDown}
        />
        <p aria-live="polite" className="t-body mt-6 max-w-[42ch]">
          {scenario.says}
        </p>
      </div>

      <div ref={ref} className="lg:col-span-8">
        <StepList steps={STEPS[id]} />
        <div className="hidden md:block">
          <DiagramFigure label={c.diagramLabel}>
            <svg viewBox="0 0 736 280" className="block h-auto w-full max-w-[736px]" fill="none">
              <Wire
                points={[
                  [156, 68],
                  [216, 68],
                ]}
                on={id === "replicate"}
              />
              <Wire
                points={[
                  [386, 56],
                  [496, 56],
                ]}
                on={id === "replicate"}
                label={{ x: 441, y: 40, text: "copy" }}
              />
              <Wire
                points={[
                  [496, 84],
                  [386, 84],
                ]}
                on={id === "repair"}
                label={{ x: 441, y: 112, text: "repair" }}
              />
              <Wire
                points={[
                  [301, 204],
                  [301, 96],
                ]}
                on={id === "repair"}
                label={{ x: 254, y: 154, text: "re-read" }}
              />
              <Wire
                points={[
                  [560, 204],
                  [560, 150],
                  [350, 150],
                  [350, 96],
                ]}
                on={id === "failover"}
              />
              <Wire
                points={[
                  [656, 204],
                  [656, 96],
                ]}
                on={id === "failover"}
                label={{ x: 698, y: 154, text: "failover" }}
              />

              <Box x={16} y={40} w={140} h={56} title={c.nodes.writer} state={id === "replicate" ? "on" : "off"} />
              <Box
                x={216}
                y={40}
                w={170}
                h={56}
                title={c.nodes.primary}
                sub={id === "failover" ? "not answering" : id === "repair" ? "chunk rewritten" : undefined}
                state={id === "failover" ? "off" : id === "repair" ? "end" : "on"}
              />
              <Box x={496} y={40} w={224} h={56} title={c.nodes.replica} state={id === "repair" ? "on" : "end"} />
              <Box x={216} y={204} w={170} h={56} title={c.nodes.scrub} state={id === "repair" ? "on" : "off"} />
              <Box x={496} y={204} w={224} h={56} title={c.nodes.reader} state={id === "failover" ? "on" : "off"} />
              {id === "failover" && <Mark kind="failed" x={392} y={20} size={20} />}
              <Token x={ex - 8} y={ey - 8} />
            </svg>
          </DiagramFigure>
        </div>
      </div>
    </div>
  );
}
