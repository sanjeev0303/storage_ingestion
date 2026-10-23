"use client";

import { copy } from "../copy";
import {
  DiagramFigure,
  Mark,
  Token,
  useDiagramRef,
  useDiagramTimeline,
} from "../diagram-kit";

type State = "ahead" | "current" | "passed";

const NODE_STATES: Record<
  State,
  { solid: number; dashed: number; ink: number; amber: number }
> = {
  ahead: { solid: 0, dashed: 1, ink: 0, amber: 0 },
  current: { solid: 1, dashed: 0, ink: 0, amber: 1 },
  passed: { solid: 1, dashed: 0, ink: 1, amber: 0 },
};

const SPINE_X = 110;
const SIDE_X = 290;
const ROW_GAP = 115;
const FIRST_Y = 60;

// The picture as it ends: the connection is the default, the side branches are ahead.
const NODES = [
  {
    id: "created",
    cx: SPINE_X,
    cy: FIRST_Y,
    name: "Created",
    sub: "Pending",
    state: "passed",
  },
  {
    id: "test",
    cx: SPINE_X,
    cy: FIRST_Y + ROW_GAP,
    name: "Test",
    state: "passed",
  },
  {
    id: "active",
    cx: SPINE_X,
    cy: FIRST_Y + ROW_GAP * 2,
    name: "Active",
    state: "passed",
  },
  {
    id: "default",
    cx: SPINE_X,
    cy: FIRST_Y + ROW_GAP * 3,
    name: "Default",
    state: "current",
  },
  {
    id: "disabled",
    cx: SIDE_X,
    cy: FIRST_Y + ROW_GAP * 2,
    name: "Disabled",
    state: "ahead",
  },
  { id: "delete", cx: SIDE_X, cy: 520, name: "Delete", state: "ahead" },
] as const;

function Node({ node }: { node: (typeof NODES)[number] }) {
  const { id, cx, cy, name, state } = node;
  const sub = "sub" in node ? node.sub : undefined;
  const onSpine = cx === SPINE_X;
  const x = cx - 11;
  const y = cy - 11;
  const initial = NODE_STATES[state];
  return (
    <g data-node={id}>
      <rect
        data-solid
        x={x}
        y={y}
        width="22"
        height="22"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        opacity={initial.solid}
      />
      <rect
        data-dashed
        x={x}
        y={y}
        width="22"
        height="22"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeDasharray="4 3"
        opacity={initial.dashed}
      />
      <rect
        data-ink
        x={x}
        y={y}
        width="22"
        height="22"
        fill="currentColor"
        opacity={initial.ink}
      />
      <rect
        data-amber
        x={x}
        y={y}
        width="22"
        height="22"
        fill="var(--accent)"
        stroke="currentColor"
        strokeWidth="2"
        opacity={initial.amber}
      />
      <text
        x={onSpine ? cx - 24 : cx + 24}
        y={sub ? cy - 2 : cy + 6}
        textAnchor={onSpine ? "end" : "start"}
        fontSize="18"
        className="font-semibold"
      >
        {name}
      </text>
      {sub && (
        <text x={cx - 24} y={cy + 18} textAnchor="end" fontSize="16">
          {sub}
        </text>
      )}
    </g>
  );
}

const edge = { fill: "none", stroke: "currentColor", strokeWidth: 2 } as const;

export function ConnectionLifecycle() {
  const ref = useDiagramRef();

  useDiagramTimeline(ref, (tl, root) => {
    const token = root.querySelector("[data-token]");
    const part = (node: string, kind: string) =>
      root.querySelector(`[data-node="${node}"] [data-${kind}]`);
    const labels = root.querySelectorAll("[data-label]");
    const label = (name: string) =>
      root.querySelector(`[data-label="${name}"]`);

    const setNode = (
      node: string,
      state: State,
      at: number,
      duration = 0.2,
    ) => {
      const target = NODE_STATES[state];
      tl.to(part(node, "solid"), { opacity: target.solid, duration }, at);
      tl.to(part(node, "dashed"), { opacity: target.dashed, duration }, at);
      tl.to(part(node, "ink"), { opacity: target.ink, duration }, at);
      tl.to(part(node, "amber"), { opacity: target.amber, duration }, at);
    };

    // Start state: Created is current, everything else is still ahead.
    tl.set(token, { x: 0, y: -ROW_GAP * 3 }, 0);
    tl.set(labels, { opacity: 0 }, 0);
    NODES.forEach((node) =>
      setNode(node.id, node.id === "created" ? "current" : "ahead", 0, 0),
    );

    let t = 0.6;
    const walk = (to: { x?: number; y?: number }, duration = 0.5) => {
      tl.to(token, { ...to, duration }, t);
      t += duration;
    };
    const show = (name: string) =>
      tl.to(label(name), { opacity: 1, duration: 0.25 }, t);

    // First test fails: the token shakes and goes back to Pending.
    walk({ y: -ROW_GAP * 2 });
    setNode("created", "passed", t);
    setNode("test", "current", t);
    show("fail");
    tl.to(
      token,
      { keyframes: { x: [-5, 5, -4, 4, 0], easeEach: "none" }, duration: 0.35 },
      t,
    );
    t += 0.6;
    walk({ y: -ROW_GAP * 3 }, 0.45);
    setNode("test", "ahead", t);
    setNode("created", "current", t);
    t += 0.4;

    // Second test passes.
    walk({ y: -ROW_GAP * 2 });
    setNode("created", "passed", t);
    setNode("test", "current", t);
    show("pass");
    t += 0.3;
    walk({ y: -ROW_GAP });
    setNode("test", "passed", t);
    setNode("active", "current", t);
    t += 0.4;

    // Disable, then enable, which tests again.
    show("disable");
    walk({ x: SIDE_X - SPINE_X });
    setNode("active", "ahead", t);
    setNode("disabled", "current", t);
    t += 0.5;
    show("enable");
    walk({ y: -ROW_GAP * 2 });
    setNode("disabled", "ahead", t);
    setNode("test", "current", t);
    walk({ x: 0 });
    t += 0.2;
    walk({ y: -ROW_GAP });
    setNode("test", "passed", t);
    setNode("active", "current", t);
    t += 0.4;

    // Make it the default.
    show("makeDefault");
    walk({ y: 0 });
    setNode("active", "passed", t);
    setNode("default", "current", t);
  });

  return (
    <div ref={ref}>
      <DiagramFigure label={copy.connect.diagramLabel}>
        <svg
          viewBox="0 0 420 580"
          className="block h-auto w-full overflow-visible"
          focusable="false"
        >
          <defs>
            <marker
              id="lifecycle-arrow"
              viewBox="0 0 10 10"
              refX="9"
              refY="5"
              markerWidth="10"
              markerHeight="10"
              markerUnits="userSpaceOnUse"
              orient="auto"
            >
              <path d="M0 0L10 5L0 10z" fill="currentColor" />
            </marker>
          </defs>

          <g {...edge} markerEnd="url(#lifecycle-arrow)">
            <path d="M110 71V164" />
            <path d="M110 186V279" />
            <path d="M110 301V394" />
            <path d="M121 290H279" />
            <path d="M290 279V182H121" />
            <path d="M121 168C205 168 205 65 121 65" />
            <path d="M290 301V509" />
          </g>
          <path d="M121 53H408V470H290" {...edge} />

          <text x="140" y="40" fontSize="16">
            Delete from Pending or Disabled
          </text>
          <text x="266" y="515" textAnchor="end" fontSize="16">
            Never while objects
          </text>
          <text x="266" y="535" textAnchor="end" fontSize="16">
            depend on it
          </text>

          <g data-label="fail">
            <Mark kind="failed" x={200} y={96} />
            <text x="222" y="109" fontSize="16">
              Test fails
            </text>
            <text x="200" y="130" fontSize="16">
              stays Pending
            </text>
          </g>
          <g data-label="enable">
            <text x="207" y="204" textAnchor="middle" fontSize="16">
              Enable, then test
            </text>
          </g>
          <g data-label="pass">
            <Mark kind="passed" x={130} y={235} />
            <text x="152" y="248" fontSize="16">
              Test passes
            </text>
          </g>
          <g data-label="disable">
            <text x="200" y="280" textAnchor="middle" fontSize="16">
              Disable
            </text>
          </g>
          <g data-label="makeDefault">
            <text x="130" y="357" fontSize="16">
              Make default
            </text>
          </g>

          {NODES.map((node) => (
            <Node key={node.id} node={node} />
          ))}

          <Token x={SPINE_X - 8} y={FIRST_Y + ROW_GAP * 3 - 8} size={16} />
        </svg>
      </DiagramFigure>
    </div>
  );
}
