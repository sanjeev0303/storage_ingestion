"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, useState, type KeyboardEvent } from "react";
import { cn } from "@/lib/utils/cn";
import { copy } from "../copy";
import { Token, useDiagramRef, useDiagramTimeline } from "../diagram-kit";

gsap.registerPlugin(useGSAP, ScrollTrigger);

const [CONTAINS, DERIVED_FROM, IMPORTED_FROM, VALIDATED_BY, , TRANSFORMED_BY] =
  copy.lineage.relations;

const NODE_W = 160;
const NODE_H = 54;

type NodeId =
  | "manifest"
  | "transformation"
  | "dataset"
  | "v1"
  | "v2"
  | "reportA"
  | "reportB";

const nodes: {
  id: NodeId;
  name: string;
  sub?: string;
  cx: number;
  y: number;
}[] = [
  { id: "manifest", name: "Manifest import", cx: 100, y: 20 },
  { id: "transformation", name: "Transformation", cx: 310, y: 20 },
  { id: "dataset", name: "Dataset", cx: 100, y: 140 },
  { id: "v1", name: "Version 1", cx: 100, y: 270 },
  { id: "v2", name: "Version 2", cx: 310, y: 380 },
  {
    id: "reportA",
    name: "Report A",
    sub: "validates Version 1",
    cx: 100,
    y: 540,
  },
  {
    id: "reportB",
    name: "Report B",
    sub: "validates Version 2",
    cx: 310,
    y: 540,
  },
];

const nameOf = (id: NodeId) => nodes.find((n) => n.id === id)!.name;

// from, relation and to read as the API states them. up and down say which end
// is the source, so upstream and downstream mean the same on every edge.
// Paths are drawn from the up node to the down node.
const edges: {
  id: string;
  from: NodeId;
  relation: string;
  to: NodeId;
  up: NodeId;
  down: NodeId;
  d: string;
  label: { x: number; y: number; end?: boolean };
}[] = [
  {
    id: "imp",
    from: "dataset",
    relation: IMPORTED_FROM,
    to: "manifest",
    up: "manifest",
    down: "dataset",
    d: "M100 74V140",
    label: { x: 110, y: 112 },
  },
  {
    id: "c1",
    from: "dataset",
    relation: CONTAINS,
    to: "v1",
    up: "dataset",
    down: "v1",
    d: "M100 194V270",
    label: { x: 110, y: 237 },
  },
  {
    id: "c2",
    from: "dataset",
    relation: CONTAINS,
    to: "v2",
    up: "dataset",
    down: "v2",
    d: "M180 167C270 167 270 250 270 380",
    label: { x: 280, y: 300 },
  },
  {
    id: "der",
    from: "v2",
    relation: DERIVED_FROM,
    to: "v1",
    up: "v1",
    down: "v2",
    d: "M180 297C205 297 205 407 230 407",
    label: { x: 188, y: 358, end: true },
  },
  {
    id: "tr",
    from: "v2",
    relation: TRANSFORMED_BY,
    to: "transformation",
    up: "transformation",
    down: "v2",
    d: "M350 74V380",
    label: { x: 342, y: 130, end: true },
  },
  {
    id: "va1",
    from: "v1",
    relation: VALIDATED_BY,
    to: "reportA",
    up: "v1",
    down: "reportA",
    d: "M100 324V540",
    label: { x: 110, y: 440 },
  },
  {
    id: "va2",
    from: "v2",
    relation: VALIDATED_BY,
    to: "reportB",
    up: "v2",
    down: "reportB",
    d: "M310 434V540",
    label: { x: 320, y: 492 },
  },
];

type Direction = "upstream" | "downstream";
type Edge = (typeof edges)[number];

function walk(start: NodeId, direction: Direction) {
  const found: Edge[] = [];
  const queue = [start];
  while (queue.length > 0) {
    const node = queue.shift()!;
    for (const edge of edges) {
      const here = direction === "upstream" ? edge.down : edge.up;
      const next = direction === "upstream" ? edge.up : edge.down;
      if (here !== node || found.includes(edge)) continue;
      found.push(edge);
      queue.push(next);
    }
  }
  return found;
}

const sentence = (edge: Edge) =>
  `${nameOf(edge.from)} ${edge.relation.toLowerCase()} ${nameOf(edge.to)}`;

export function LineageGraph() {
  const [start, setStart] = useState<NodeId>("v2");
  const [direction, setDirection] = useState<Direction>("upstream");
  const root = useDiagramRef();
  const svg = useRef<SVGSVGElement>(null);

  const walked = walk(start, direction);
  const walkedIds = walked.map((edge) => edge.id).join(",");

  useDiagramTimeline(
    root,
    (tl, el) => {
      tl.from(el.querySelectorAll("[data-edge]"), {
        drawSVG: 0,
        duration: 0.7,
        ease: "power2.inOut",
        stagger: 0.12,
      });
      tl.from(
        el.querySelectorAll("[data-node]"),
        { scale: 0.9, autoAlpha: 0, duration: 0.4, stagger: 0.08 },
        0,
      );
    },
  );

  // The amber token walks the marked edges, one after another, as the reader scrolls past the graph.
  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        const token = svg.current?.querySelector("[data-token]");
        if (!token) return;
        const tl = gsap.timeline({ paused: true });
        for (const edge of walked) {
          const path = svg.current?.querySelector<SVGPathElement>(`[data-edge="${edge.id}"]`);
          if (!path) continue;
          const upstream = direction === "upstream";
          tl.set(token, { autoAlpha: 1 });
          tl.to(token, {
            motionPath: {
              path,
              align: path,
              alignOrigin: [0.5, 0.5],
              start: upstream ? 1 : 0,
              end: upstream ? 0 : 1,
            },
            duration: 0.7,
            ease: "power1.inOut",
          });
        }

        const trigger = ScrollTrigger.create({
          trigger: svg.current,
          animation: tl,
          start: "top 70%",
          end: "bottom 45%",
          scrub: 0.5,
        });
        return () => {
          trigger.kill();
          tl.kill();
        };
      });
    },
    { scope: root, dependencies: [walkedIds, direction], revertOnUpdate: true },
  );

  function onNodeKey(event: KeyboardEvent<SVGGElement>, id: NodeId) {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    setStart(id);
  }

  return (
    <div>
      <div role="group" aria-label="Direction" className="flex">
        {copy.lineage.directions.map((label) => {
          const value = label.toLowerCase() as Direction;
          const pressed = direction === value;
          return (
            <button
              key={label}
              type="button"
              aria-pressed={pressed}
              onClick={() => setDirection(value)}
              className={cn(
                "h-12 border border-current px-8 font-semibold transition-colors duration-150 ease-out active:scale-[0.97]",
                pressed ? "bg-accent text-on-accent" : "hover:bg-ground/10",
              )}
            >
              {label}
            </button>
          );
        })}
      </div>
      <p aria-live="polite" className="t-small mt-3">
        Walking {direction} from {nameOf(start)}: {walked.length}{" "}
        {walked.length === 1 ? "relation" : "relations"}.
      </p>

      <figure className="diagram m-0 mt-4">
        <div ref={root}>
          <svg
            ref={svg}
            viewBox="0 0 420 620"
            role="group"
            aria-label="Lineage graph"
            className="h-auto w-full max-w-[460px]"
          >
            {edges.map((edge) => {
              const isWalked = walked.includes(edge);
              return (
                <g key={edge.id} aria-hidden>
                  <path
                    data-edge={edge.id}
                    data-walked={isWalked}
                    d={edge.d}
                    fill="none"
                    stroke="currentColor"
                    strokeWidth={isWalked ? 2.5 : 1.5}
                    strokeOpacity={isWalked ? 1 : 0.4}
                  />
                  <text
                    x={edge.label.x}
                    y={edge.label.y}
                    fontSize={15}
                    textAnchor={edge.label.end ? "end" : "start"}
                    stroke="var(--ink)"
                    strokeWidth={5}
                    style={{ paintOrder: "stroke" }}
                  >
                    {edge.relation}
                  </text>
                </g>
              );
            })}

            {nodes.map((node) => {
              const selected = node.id === start;
              const x = node.cx - NODE_W / 2;
              return (
                <g
                  key={node.id}
                  data-node={node.id}
                  data-start={selected}
                  role="button"
                  tabIndex={0}
                  aria-pressed={selected}
                  aria-label={`${node.name}, start node`}
                  onClick={() => setStart(node.id)}
                  onKeyDown={(event) => onNodeKey(event, node.id)}
                  className="cursor-pointer focus-visible:outline-2 focus-visible:outline-offset-4 focus-visible:outline-current"
                >
                  <rect
                    x={x}
                    y={node.y}
                    width={NODE_W}
                    height={NODE_H}
                    fill={selected ? "var(--accent)" : "var(--ink)"}
                    stroke="currentColor"
                    strokeWidth={selected ? 3 : 1.5}
                  />
                  <text
                    x={node.cx}
                    y={node.y + (node.sub ? 24 : 33)}
                    fontSize={16}
                    fontWeight={600}
                    textAnchor="middle"
                    style={selected ? { fill: "var(--on-accent)" } : undefined}
                  >
                    {node.name}
                  </text>
                  {node.sub && (
                    <text
                      x={node.cx}
                      y={node.y + 43}
                      fontSize={13}
                      textAnchor="middle"
                      style={
                        selected ? { fill: "var(--on-accent)" } : undefined
                      }
                    >
                      {node.sub}
                    </text>
                  )}
                </g>
              );
            })}

            <g aria-hidden pointerEvents="none">
              <Token x={-7} y={-7} size={14} className="opacity-0" />
            </g>
          </svg>
        </div>
        <figcaption className="t-small mt-3 opacity-90">
          {copy.lineage.caption}
        </figcaption>
      </figure>

      <ul className="sr-only" aria-label="Lineage relations">
        {edges.map((edge) => (
          <li key={edge.id}>
            {sentence(edge)}
            {walked.includes(edge) ? ", on the walked path" : ""}
          </li>
        ))}
      </ul>
    </div>
  );
}
