"use client";

import gsap from "gsap";
import { copy } from "../copy";
import { Mark, Token, useDiagramRef, useDiagramTimeline } from "../diagram-kit";

type Orientation = "row" | "column";

type Detail = { text: string; kind?: "pass" | "fail" };

// What the console records at each stage, as small mono lines under its name.
const details: Detail[][] = [
  [{ text: "in quarantine" }],
  [
    { text: "size matches" },
    { text: "sha256 matches", kind: "pass" },
    { text: "sha256 differs", kind: "fail" },
  ],
  [{ text: "scan clean" }],
  [{ text: "moved out" }],
  [{ text: "listed" }],
];

const last = copy.verify.stages.length - 1;
const CHIP = { w: 128, h: 28 };

const layouts = {
  row: {
    viewBox: "0 0 1100 280",
    node: 56,
    pos: (i: number) => ({ x: 110 + 220 * i, y: 90 }),
    chip: { x: 0, y: -62 },
    label: (p: { x: number; y: number }) => ({
      x: p.x,
      y: 162,
      anchor: "middle" as const,
    }),
    box: { x: 306, y: 214, w: 290, h: 52 },
    mark: { x: 318, y: 228 },
    boxText: { x: 352, y: 245 },
    boxTokenFrom: { x: 330, y: 240 },
  },
  column: {
    viewBox: "0 0 420 620",
    node: 44,
    pos: (i: number) => ({ x: 60, y: 60 + 120 * i }),
    chip: { x: 220, y: 0 },
    label: (p: { x: number; y: number }) => ({
      x: 110,
      y: p.y - 4,
      anchor: "start" as const,
    }),
    box: { x: 215, y: 232, w: 195, h: 54 },
    mark: { x: 228, y: 247 },
    boxText: { x: 262, y: 255 },
    boxTokenFrom: { x: 240, y: 259 },
  },
};

export function VerifyPipeline({ orientation }: { orientation: Orientation }) {
  const ref = useDiagramRef();
  const layout = layouts[orientation];
  const isRow = orientation === "row";
  const end = layout.pos(last);
  const [rejectedHead, rejectedReason] = copy.verify.rejected.split(", ");

  useDiagramTimeline(ref, (tl, root) => {
    const q = (selector: string) =>
      gsap.utils.toArray<SVGElement>(selector, root);
    const offset = (i: number) => {
      const p = layout.pos(i);
      return { x: p.x - end.x, y: p.y - end.y };
    };
    const movers = q("[data-token], [data-chipwrap]");
    const token = q("[data-token]");
    const chips = [
      q("[data-chip='0']"),
      q("[data-chip='1']"),
      q("[data-chip='2']"),
    ];
    const rejected = q("[data-rejected]");
    const rejectedMark = q("[data-rejected-mark]");

    const reset = (at: number) => {
      tl.set(q("[data-fill]"), { scaleY: 0 }, at);
      tl.set(q("[data-detail]"), { autoAlpha: 0 }, at);
      tl.set(rejected, { autoAlpha: 0 }, at);
      tl.set(rejectedMark, { autoAlpha: 0 }, at);
      tl.set(token, { autoAlpha: 1 }, at);
      tl.set(movers, offset(0), at);
      tl.set(chips[0], { autoAlpha: 1 }, at);
      tl.set([...chips[1], ...chips[2]], { autoAlpha: 0 }, at);
    };

    const swapChip = (from: number, to: number, at: number) => {
      tl.to(chips[from], { autoAlpha: 0, duration: 0.2 }, at);
      tl.to(chips[to], { autoAlpha: 1, duration: 0.2 }, at);
    };

    const run = (start: number, fails: boolean) => {
      reset(start);
      let t = start + 0.4;
      for (let i = 0; i <= last; i++) {
        if (i > 0) {
          tl.to(
            movers,
            { ...offset(i), duration: 0.7, ease: "power2.inOut" },
            t,
          );
          t += 0.7;
        }
        if (i === 1) swapChip(0, 1, t);

        if (fails && i === 1) {
          tl.to(
            q("[data-detail='1']:not([data-pass])"),
            { autoAlpha: 1, duration: 0.3, stagger: 0.2 },
            t,
          );
          t += 0.9;
          tl.to(chips[1], { autoAlpha: 0, duration: 0.2 }, t);
          tl.to(rejected, { autoAlpha: 1, duration: 0.3 }, t);
          const to = {
            x: layout.boxTokenFrom.x - end.x,
            y: layout.boxTokenFrom.y - end.y,
          };
          if (isRow) {
            tl.to(token, { y: to.y, duration: 0.5, ease: "power2.in" }, t);
          } else {
            tl.to(token, { y: to.y, duration: 0.5, ease: "power2.in" }, t);
            tl.to(token, { x: to.x, duration: 0.4 }, t + 0.5);
            t += 0.4;
          }
          t += 0.5;
          tl.to(token, { autoAlpha: 0, duration: 0.15 }, t);
          tl.to(rejectedMark, { autoAlpha: 1, duration: 0.2 }, t);
          return t + 0.2;
        }

        tl.to(
          q(`[data-detail='${i}']:not([data-fail])`),
          { autoAlpha: 1, duration: 0.3, stagger: 0.15 },
          t,
        );
        tl.to(q(`[data-fill='${i}']`), { scaleY: 1, duration: 0.35 }, t);
        if (i === last) swapChip(1, 2, t);
        t += 0.5;
      }
      return t;
    };

    const endOfRunA = run(0, false);
    tl.to({}, { duration: 1.2 }, endOfRunA);
    run(endOfRunA + 1.2, true);
  });

  const nodeHalf = layout.node / 2;
  const stages = copy.verify.stages;

  return (
    <div ref={ref}>
      <svg
        viewBox={layout.viewBox}
        className="block h-auto w-full"
        focusable="false"
      >
        {stages.slice(1).map((name, k) => {
          const a = layout.pos(k);
          const b = layout.pos(k + 1);
          return (
            <line
              key={name}
              x1={isRow ? a.x + nodeHalf : a.x}
              x2={isRow ? b.x - nodeHalf : b.x}
              y1={isRow ? a.y : a.y + nodeHalf}
              y2={isRow ? b.y : b.y - nodeHalf}
              stroke="currentColor"
              strokeWidth="2"
            />
          );
        })}

        {stages.map((name, i) => {
          const p = layout.pos(i);
          const label = layout.label(p);
          return (
            <g key={name}>
              <rect
                x={p.x - nodeHalf}
                y={p.y - nodeHalf}
                width={layout.node}
                height={layout.node}
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
              />
              <rect
                data-fill={i}
                x={p.x - nodeHalf}
                y={p.y - nodeHalf}
                width={layout.node}
                height={layout.node}
                fill="currentColor"
                style={{
                  transformBox: "fill-box",
                  transformOrigin: "50% 100%",
                }}
              />
              <text
                x={label.x}
                y={label.y}
                textAnchor={label.anchor}
                fontSize="18"
                fontWeight="600"
              >
                {name}
              </text>
              {details[i].map((detail, k) => (
                <text
                  key={detail.text}
                  data-detail={i}
                  data-pass={detail.kind === "pass" ? "" : undefined}
                  data-fail={detail.kind === "fail" ? "" : undefined}
                  x={label.x}
                  y={label.y + 24 + 21 * (detail.kind === "fail" ? 1 : k)}
                  textAnchor={label.anchor}
                  fontSize="14"
                  opacity={detail.kind === "fail" ? 0 : undefined}
                >
                  {detail.text}
                </text>
              ))}
            </g>
          );
        })}

        <g data-rejected opacity="0">
          <rect
            x={layout.box.x}
            y={layout.box.y}
            width={layout.box.w}
            height={layout.box.h}
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
          />
          {isRow ? (
            <text
              x={layout.boxText.x}
              y={layout.boxText.y}
              fontSize="16"
              fontWeight="600"
            >
              {copy.verify.rejected}
            </text>
          ) : (
            <text
              x={layout.boxText.x}
              y={layout.boxText.y}
              fontSize="16"
              fontWeight="600"
            >
              <tspan>{rejectedHead}</tspan>
              <tspan x={layout.boxText.x} dy="20" fontWeight="400">
                {rejectedReason}
              </tspan>
            </text>
          )}
        </g>
        <g data-rejected-mark opacity="0">
          <Mark kind="failed" x={layout.mark.x} y={layout.mark.y} size={24} />
        </g>

        <g transform={`translate(${end.x} ${end.y})`}>
          <Token x={-14} y={-14} size={28} />
          <g transform={`translate(${layout.chip.x} ${layout.chip.y})`}>
            <g data-chipwrap>
              {copy.verify.states.map((state, k) => (
                <g key={state} data-chip={k} opacity={k === 2 ? undefined : 0}>
                  <rect
                    x={isRow ? -CHIP.w / 2 : 0}
                    y={-CHIP.h / 2}
                    width={CHIP.w}
                    height={CHIP.h}
                    fill={k === 2 ? "var(--accent)" : "none"}
                    stroke="currentColor"
                    strokeWidth="2"
                  />
                  <text
                    className="mono"
                    x={isRow ? 0 : CHIP.w / 2}
                    y="5"
                    textAnchor="middle"
                    fontSize="14"
                    style={k === 2 ? { fill: "var(--on-accent)" } : undefined}
                  >
                    {state}
                  </text>
                </g>
              ))}
            </g>
          </g>
        </g>
      </svg>
    </div>
  );
}
