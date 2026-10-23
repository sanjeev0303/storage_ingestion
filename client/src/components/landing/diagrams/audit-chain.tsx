"use client";

import { copy } from "../copy";
import {
  DiagramFigure,
  Mark,
  Token,
  useDiagramRef,
  useDiagramTimeline,
} from "../diagram-kit";

const EVENTS = [
  { action: "auth.login", hash: "7d2e91" },
  { action: "member.role_change", hash: "91ac40" },
  { action: "asset.delete", hash: "40f7b3" },
  { action: "retention.policy_set", hash: "b30e5d" },
  { action: "auth.logout", hash: "5d18c2" },
];

const BROKEN_AT = 4;
const top = (index: number) => 10 + index * 86;

export function AuditChain() {
  const ref = useDiagramRef();

  useDiagramTimeline(ref, (tl, root) => {
    const pick = (selector: string) =>
      Array.from(root.querySelectorAll(selector));
    const token = pick("[data-token]");
    const passes = pick("[data-pass]");
    const finals = pick("[data-final]");
    const intactTrue = pick("[data-intact-true]");
    const intactFalse = pick("[data-intact-false]");
    // The token rests on the broken block in the markup.
    const offset = (index: number) => (index - (BROKEN_AT - 1)) * 86;

    tl.set(token, { y: offset(0) });
    tl.set([...passes, ...finals, ...intactTrue, ...intactFalse], {
      autoAlpha: 0,
    });

    EVENTS.forEach((_, i) => {
      tl.to(token, { y: offset(i), duration: 0.45 }, i === 0 ? "+=0.4" : ">");
      tl.to(passes[i], { autoAlpha: 1, duration: 0.2 });
    });
    tl.to(intactTrue, { autoAlpha: 1, duration: 0.3 });
    tl.to([...passes, ...intactTrue], { autoAlpha: 0, duration: 0.3 }, "+=1.2");

    EVENTS.forEach((_, i) => {
      tl.to(token, { y: offset(i), duration: 0.45 });
      if (i < BROKEN_AT - 1) tl.to(passes[i], { autoAlpha: 1, duration: 0.2 });
      if (i === BROKEN_AT - 1)
        tl.to([finals[0], finals[1]], { autoAlpha: 1, duration: 0.2 });
      if (i === BROKEN_AT) tl.to(finals[2], { autoAlpha: 1, duration: 0.2 });
    });
    tl.to(intactFalse, { autoAlpha: 1, duration: 0.3 });
  });

  return (
    <DiagramFigure label={copy.access.diagramLabel}>
      <div ref={ref}>
        <svg
          viewBox="0 0 420 456"
          className="mx-auto block h-auto w-full max-w-[460px]"
          fill="none"
        >
          {EVENTS.map((event, i) => {
            const y = top(i);
            const order = i + 1;
            return (
              <g key={event.action}>
                <rect
                  x="34"
                  y={y}
                  width="366"
                  height="56"
                  stroke="currentColor"
                  strokeWidth="2"
                />
                <text x="48" y={y + 24} className="mono" fontSize="16">
                  {event.action}
                </text>
                <text x="48" y={y + 44} className="mono" fontSize="14">
                  {`hash ${event.hash}`}
                </text>
                <text x="350" y={y + 24} fontSize="14" textAnchor="end">
                  {`event ${order}`}
                </text>
                <g data-pass opacity={order >= BROKEN_AT ? 0 : 1}>
                  <Mark kind="passed" x={366} y={y + 12} size={20} />
                </g>
                {order === BROKEN_AT && (
                  <g data-final>
                    <Mark kind="failed" x={366} y={y + 12} size={20} />
                  </g>
                )}
                {order === BROKEN_AT + 1 && (
                  <g data-final>
                    <Mark kind="not_assessed" x={366} y={y + 12} size={20} />
                  </g>
                )}
                {order < EVENTS.length && (
                  <>
                    <line
                      x1="60"
                      x2="60"
                      y1={y + 56}
                      y2={y + 86}
                      stroke="currentColor"
                      strokeWidth="2"
                    />
                    <text x="80" y={y + 76} className="mono" fontSize="14">
                      {`prev ${event.hash.slice(-2)}`}
                    </text>
                  </>
                )}
                {order === BROKEN_AT - 1 && (
                  <g data-final>
                    <Mark kind="failed" x={52} y={y + 63} />
                  </g>
                )}
              </g>
            );
          })}
          <Token x={6} y={top(BROKEN_AT - 1) + 20} />
          <text
            data-intact-true
            opacity="0"
            x="34"
            y="446"
            className="mono"
            fontSize="16"
          >
            intact: true
          </text>
          <text data-intact-false x="34" y="446" className="mono" fontSize="16">
            intact: false, broken at event 4
          </text>
        </svg>
      </div>
    </DiagramFigure>
  );
}
