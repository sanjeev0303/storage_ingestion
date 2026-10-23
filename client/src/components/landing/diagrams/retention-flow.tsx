"use client";

import { copy } from "../copy";
import {
  DiagramFigure,
  Token,
  useDiagramRef,
  useDiagramTimeline,
} from "../diagram-kit";

const LINE_X = 280;
const BOX = { x: 170, w: 220, h: 44 };

function Box({ y, title }: { y: number; title: string }) {
  return (
    <g>
      <rect
        x={BOX.x}
        y={y}
        width={BOX.w}
        height={BOX.h}
        fill="var(--faint)"
        stroke="currentColor"
        strokeWidth="2"
      />
      <text
        x={LINE_X}
        y={y + 28}
        textAnchor="middle"
        fontSize="16"
        fontWeight="600"
      >
        {title}
      </text>
    </g>
  );
}

export function RetentionFlow() {
  const ref = useDiagramRef();

  useDiagramTimeline(ref, (tl, root) => {
    const token = root.querySelector("[data-token]");
    const restore = root.querySelector("[data-restore]");
    const held = root.querySelector("[data-held]");
    // Offsets are from where the token rests in the markup: at the hold gate.
    const live = { x: 0, y: -174 };
    const grace = { x: 0, y: -72 };
    const left = { x: -240, y: -72 };
    const leftTop = { x: -240, y: -174 };

    tl.set(token, live);
    tl.set([restore, held], { autoAlpha: 0 });
    tl.to(token, { ...grace, duration: 0.7 }, "+=0.4");
    tl.to(token, { ...left, duration: 0.5 }, "+=0.3");
    tl.to(restore, { autoAlpha: 1, duration: 0.3 }, "<");
    tl.to(token, { ...leftTop, duration: 0.6 });
    tl.to(token, { ...live, duration: 0.5 });
    tl.to(restore, { autoAlpha: 0, duration: 0.3 }, "<0.2");
    tl.to(token, { ...grace, duration: 0.7 }, "+=0.5");
    tl.to(token, { x: 0, y: 0, duration: 0.7, ease: "power2.in" }, "+=0.3");
    tl.to(held, { autoAlpha: 1, duration: 0.3 });
  });

  return (
    <DiagramFigure label={copy.retain.diagramLabel}>
      <div ref={ref}>
        <svg
          viewBox="0 0 420 360"
          className="mx-auto block h-auto w-full max-w-[460px]"
          fill="none"
        >
          <Box y={10} title="Live" />
          <Box y={110} title="Grace period" />
          <Box y={290} title="Purged" />

          <g stroke="currentColor" strokeWidth="2">
            <path d="M280 54V110M280 154V290" />
            <path d="M270 100l10 10 10-10M270 280l10 10 10-10" />
          </g>
          <text x={LINE_X + 14} y={88} fontSize="15">
            Delete
          </text>

          <g data-restore>
            <path
              d="M170 132H40V32H170"
              stroke="currentColor"
              strokeWidth="2"
              strokeDasharray="6 4"
            />
            <path
              d="M160 22l10 10-10 10"
              stroke="currentColor"
              strokeWidth="2"
            />
            <text x="52" y="84" fontSize="15">
              Restore
            </text>
          </g>

          <rect x="236" y="212" width="88" height="14" fill="currentColor" />
          <text x="40" y="224" fontSize="15">
            Hold or retention rule
          </text>
          <text
            data-held
            x={LINE_X + 56}
            y="256"
            fontSize="15"
            textAnchor="start"
          >
            Kept
          </text>

          <Token x={LINE_X - 8} y={196} />
        </svg>
      </div>
    </DiagramFigure>
  );
}
