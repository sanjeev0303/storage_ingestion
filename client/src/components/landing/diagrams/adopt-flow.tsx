"use client";

import { useRef, useState } from "react";
import { copy } from "../copy";
import { DiagramFigure, Token } from "../diagram-kit";
import { Box, Choices, StepList, Switch, Wire, useChoice, useTokenRun } from "../flow-kit";

const c = copy.adopt;
type SourceId = (typeof c.sources)[number]["id"];

const AT = {
  source: [96, 68],
  pin: [276, 68],
  read: [456, 68],
  match: [640, 68],
  yesTurn: [672, 148],
  yes: [672, 182],
  noTurn: [608, 148],
  noCorner: [380, 148],
  no: [380, 182],
} as const;

export function AdoptFlow() {
  const ref = useRef<HTMLDivElement>(null);
  const choice = useChoice<SourceId>("inventory");
  const [changed, setChanged] = useState(false);
  const source = c.sources.find((s) => s.id === choice.value) ?? c.sources[0];
  const upload = source.lands !== "in place";
  const path = changed
    ? [AT.source, AT.pin, AT.read, AT.match, AT.noTurn, AT.noCorner, AT.no]
    : [AT.source, AT.pin, AT.read, AT.match, AT.yesTurn, AT.yes];
  useTokenRun(ref, path, `${choice.value}-${changed}`, choice.animate);
  const [ex, ey] = path[path.length - 1];

  return (
    <div className="grid gap-10">
      <div className="grid justify-items-start gap-4">
        <Choices
          legend={c.pickLabel}
          options={c.sources.map((s) => ({ id: s.id, label: s.name }))}
          value={choice.value}
          onChoose={choice.choose}
          onKeyDown={choice.onKeyDown}
          onPointerDown={choice.onPointerDown}
        />
        <Switch
          label="One object's bytes changed"
          checked={changed}
          onChange={(value) => {
            choice.animateNext();
            setChanged(value);
          }}
        />
      </div>

      <div ref={ref}>
        <StepList
          steps={[
            { label: c.stages.source, note: source.name, state: "on" },
            { label: c.stages.pin, state: "on" },
            { label: c.stages.read, note: c.readNote, state: "on" },
            { label: c.stages.match, note: changed ? "no" : "yes", state: "on" },
            { label: changed ? c.outcomes.mismatch : upload ? c.outcomes.upload : c.outcomes.inPlace, state: "end" },
          ]}
        />
        <div className="hidden md:block">
          <DiagramFigure label={c.diagramLabel}>
            <svg viewBox="0 0 760 268" className="mx-auto block h-auto w-full max-w-[760px]" fill="none">
              <Wire
                points={[
                  [176, 68],
                  [216, 68],
                ]}
              />
              <Wire
                points={[
                  [336, 68],
                  [376, 68],
                ]}
              />
              <Wire
                points={[
                  [536, 68],
                  [572, 68],
                ]}
              />
              <Wire
                points={[
                  [672, 96],
                  [672, 200],
                ]}
                on={!changed}
                label={{ x: 706, y: 152, text: "yes" }}
              />
              <Wire
                points={[
                  [608, 96],
                  [608, 148],
                  [380, 148],
                  [380, 200],
                ]}
                on={changed}
                label={{ x: 494, y: 134, text: "no" }}
              />

              <Box x={16} y={40} w={160} h={56} title={c.stages.source} sub={source.name} />
              <Box x={216} y={40} w={120} h={56} title={c.stages.pin} />
              <Box x={376} y={40} w={160} h={56} title={c.stages.read} sub={c.readNote} />
              <Box x={572} y={40} w={136} h={56} title={c.stages.match} />
              <Box x={260} y={200} w={240} h={56} title={c.outcomes.mismatch} state={changed ? "end" : "off"} />
              <Box x={548} y={200} w={200} h={56} title={upload ? c.outcomes.upload : c.outcomes.inPlace} state={changed ? "off" : "end"} />
              <Token x={ex - 8} y={ey - 8} />
            </svg>
          </DiagramFigure>
        </div>
      </div>

      <dl aria-live="polite" className="grid gap-6 sm:grid-cols-3">
        <div>
          <dt className="t-small font-semibold">Reads</dt>
          <dd className="t-body mt-1">{source.reads}</dd>
        </div>
        <div>
          <dt className="t-small font-semibold">Brings</dt>
          <dd className="t-body mt-1">{source.brings}</dd>
        </div>
        <div>
          <dt className="t-small font-semibold">Lands</dt>
          <dd className="t-body mt-1">
            {source.lands === "in place" ? "In place: nothing is copied" : "Through an ordinary upload, then the same checks"}
          </dd>
        </div>
      </dl>
    </div>
  );
}
