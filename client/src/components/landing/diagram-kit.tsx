"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { DrawSVGPlugin } from "gsap/DrawSVGPlugin";
import { MotionPathPlugin } from "gsap/MotionPathPlugin";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, useSyncExternalStore, type ReactNode, type RefObject } from "react";
import { cn } from "@/lib/utils/cn";

gsap.registerPlugin(useGSAP, ScrollTrigger, DrawSVGPlugin, MotionPathPlugin);

type Build = (tl: gsap.core.Timeline, root: Element) => void;

// Builds one GSAP timeline for a diagram and ties it to scroll, so the flow
// plays forward as the reader scrolls down and back as they scroll up. It
// finishes while the diagram is still mid-screen, so nobody scrolls past a
// half-played diagram. The SVG in the markup is the finished picture, so
// without script, or with reduced motion, nothing is hidden and nothing moves.
export function useDiagramTimeline(scope: RefObject<Element | null>, build: Build) {
  useGSAP(
    () => {
      const root = scope.current;
      if (!root) return;
      const mm = gsap.matchMedia();

      mm.add("(prefers-reduced-motion: no-preference)", () => {
        const tl = gsap.timeline({ paused: true, defaults: { ease: "power2.out", duration: 0.5 } });
        build(tl, root);

        // Scrub maps the timeline onto the scroll range, whatever its length.
        const trigger = ScrollTrigger.create({
          trigger: root,
          animation: tl,
          start: "top 80%",
          end: "bottom 50%",
          scrub: 0.5,
        });
        return () => {
          trigger.kill();
          tl.kill();
        };
      });
    },
    { scope },
  );
}

const noReducedMotion = () => false;
function subscribeReducedMotion(onChange: () => void) {
  const media = window.matchMedia("(prefers-reduced-motion: reduce)");
  media.addEventListener("change", onChange);
  return () => media.removeEventListener("change", onChange);
}

export function usePrefersReducedMotion() {
  return useSyncExternalStore(
    subscribeReducedMotion,
    () => window.matchMedia("(prefers-reduced-motion: reduce)").matches,
    noReducedMotion,
  );
}

type FigureProps = {
  // What the diagram shows, in a sentence. Read by screen readers instead of the SVG.
  label: string;
  caption?: ReactNode;
  className?: string;
  children: ReactNode;
};

// The wrapper every diagram uses: the picture is hidden from assistive
// technology and the label stands in for it.
export function DiagramFigure({ label, caption, className, children }: FigureProps) {
  return (
    <figure className={cn("diagram m-0", className)}>
      <div role="img" aria-label={label}>
        {children}
      </div>
      {caption && <figcaption className="t-small mt-3 opacity-90">{caption}</figcaption>}
    </figure>
  );
}

export function useDiagramRef() {
  return useRef<HTMLDivElement>(null);
}

type MarkKind = "passed" | "partial" | "warning" | "failed" | "not_assessed";

// The five outcome marks, drawn inside an SVG at (x, y). Same shapes as the console.
export function Mark({ kind, x, y, size = 16 }: { kind: MarkKind; x: number; y: number; size?: number }) {
  return (
    <g transform={`translate(${x} ${y}) scale(${size / 16})`} aria-hidden>
      {kind === "passed" && <rect x="1.5" y="1.5" width="13" height="13" fill="currentColor" />}
      {kind === "partial" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <rect x="1.5" y="1.5" width="6.5" height="13" fill="currentColor" />
        </>
      )}
      {kind === "warning" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <rect x="5" y="5" width="6" height="6" fill="var(--accent)" />
        </>
      )}
      {kind === "failed" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="var(--accent)" stroke="currentColor" strokeWidth="1.5" />
          <path d="M4.5 4.5l7 7M11.5 4.5l-7 7" stroke="currentColor" strokeWidth="1.8" />
        </>
      )}
      {kind === "not_assessed" && (
        <rect
          x="1.5"
          y="1.5"
          width="13"
          height="13"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeDasharray="2.5 2"
        />
      )}
    </g>
  );
}

// The amber token: the dataset itself, moving through a hand-off. Position it
// with gsap (x, y) from its starting point.
export function Token({ x = 0, y = 0, size = 16, className }: { x?: number; y?: number; size?: number; className?: string }) {
  return (
    <rect
      data-token
      x={x}
      y={y}
      width={size}
      height={size}
      fill="var(--accent)"
      stroke="currentColor"
      strokeWidth="2"
      className={className}
    />
  );
}
