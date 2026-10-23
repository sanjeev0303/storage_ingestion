"use client";

import { MagnifyingGlassMinus, MagnifyingGlassPlus, ArrowCounterClockwise } from "@phosphor-icons/react";
import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { KIND_LABELS, NODE_H, NODE_W, RELATION_LABELS, type Layout } from "../graph";
import { nodeLabel } from "../hooks";

type View = { x: number; y: number; k: number };
const START: View = { x: 24, y: 24, k: 1 };
const MIN_K = 0.25;
const MAX_K = 3;

const clampZoom = (k: number) => Math.min(MAX_K, Math.max(MIN_K, k));

function clip(text: string, max = 24) {
  return text.length > max ? `${text.slice(0, max - 1)}...` : text;
}

type Props = {
  layout: Layout;
  labels: Record<string, string>;
  selectedKey: string | null;
  onSelect: (key: string) => void;
};

export function LineageGraph({ layout, labels, selectedKey, onSelect }: Props) {
  const [view, setView] = useState<View>(START);
  const frame = useRef<HTMLDivElement>(null);
  const drag = useRef<{ x: number; y: number } | null>(null);

  // React registers wheel listeners as passive, which cannot stop the page scrolling.
  useEffect(() => {
    const el = frame.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      const factor = e.deltaY < 0 ? 1.1 : 1 / 1.1;
      setView((v) => ({ ...v, k: clampZoom(v.k * factor) }));
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, []);

  const zoom = (factor: number) => setView((v) => ({ ...v, k: clampZoom(v.k * factor) }));
  const pan = (dx: number, dy: number) => setView((v) => ({ ...v, x: v.x + dx, y: v.y + dy }));

  function onKeyDown(e: React.KeyboardEvent) {
    const step = 40;
    if (e.key === "ArrowLeft") pan(step, 0);
    else if (e.key === "ArrowRight") pan(-step, 0);
    else if (e.key === "ArrowUp") pan(0, step);
    else if (e.key === "ArrowDown") pan(0, -step);
    else if (e.key === "+" || e.key === "=") zoom(1.2);
    else if (e.key === "-") zoom(1 / 1.2);
    else if (e.key === "0") setView(START);
    else return;
    e.preventDefault();
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Button onClick={() => zoom(1.2)} aria-label="Zoom in">
          <MagnifyingGlassPlus aria-hidden />
        </Button>
        <Button onClick={() => zoom(1 / 1.2)} aria-label="Zoom out">
          <MagnifyingGlassMinus aria-hidden />
        </Button>
        <Button onClick={() => setView(START)}>
          <ArrowCounterClockwise aria-hidden />
          Reset view
        </Button>
        <p className="text-sm text-muted">Drag to pan, scroll or use + and - to zoom, arrow keys to pan. Zoom {Math.round(view.k * 100)}%.</p>
      </div>
      <div
        ref={frame}
        role="group"
        aria-label={`Lineage graph with ${layout.nodes.length} nodes and ${layout.links.length} edges. The table view lists the same edges.`}
        tabIndex={0}
        data-lenis-prevent
        onKeyDown={onKeyDown}
        onPointerDown={(e) => {
          if ((e.target as Element).closest("[data-node]")) return;
          drag.current = { x: e.clientX, y: e.clientY };
          e.currentTarget.setPointerCapture(e.pointerId);
        }}
        onPointerMove={(e) => {
          if (!drag.current) return;
          pan(e.clientX - drag.current.x, e.clientY - drag.current.y);
          drag.current = { x: e.clientX, y: e.clientY };
        }}
        onPointerUp={() => {
          drag.current = null;
        }}
        className="neo-in h-[28rem] touch-none overflow-hidden"
      >
        <svg width="100%" height="100%">
          <defs>
            <marker id="lineage-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
              <path d="M0 0L10 5L0 10z" fill="var(--ink)" />
            </marker>
          </defs>
          <g transform={`translate(${view.x} ${view.y}) scale(${view.k})`}>
            {layout.links.map((link, i) => {
              const mid = (link.x1 + link.x2) / 2;
              return (
                <g key={i}>
                  <path
                    d={`M${link.x1} ${link.y1}C${mid} ${link.y1} ${mid} ${link.y2} ${link.x2} ${link.y2}`}
                    fill="none"
                    stroke="var(--ink)"
                    strokeWidth="1.5"
                    markerEnd="url(#lineage-arrow)"
                  />
                  <text x={mid} y={(link.y1 + link.y2) / 2 - 5} textAnchor="middle" fontSize="11" fill="var(--ink)">
                    {RELATION_LABELS[link.edge.relation]}
                  </text>
                </g>
              );
            })}
            {layout.nodes.map((node) => {
              const selected = node.key === selectedKey;
              return (
                <g
                  key={node.key}
                  data-node
                  role="button"
                  tabIndex={0}
                  aria-pressed={selected}
                  aria-label={`${KIND_LABELS[node.kind]} ${nodeLabel(node, labels)}`}
                  transform={`translate(${node.x} ${node.y})`}
                  onClick={() => onSelect(node.key)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      e.stopPropagation();
                      onSelect(node.key);
                    }
                  }}
                  className="cursor-pointer"
                >
                  <rect
                    width={NODE_W}
                    height={NODE_H}
                    rx="2"
                    fill={selected ? "var(--accent)" : "var(--ground)"}
                    stroke="var(--ink)"
                    strokeWidth={selected ? 2.5 : 1.5}
                  />
                  <text x="10" y="19" fontSize="11" fill={selected ? "var(--on-accent)" : "var(--muted)"}>
                    {KIND_LABELS[node.kind]}
                    {node.layer === 0 ? " (start)" : ""}
                  </text>
                  <text x="10" y="36" fontSize="13" fontWeight="600" fill={selected ? "var(--on-accent)" : "var(--ink)"}>
                    {clip(nodeLabel(node, labels))}
                  </text>
                </g>
              );
            })}
          </g>
        </svg>
      </div>
    </div>
  );
}
