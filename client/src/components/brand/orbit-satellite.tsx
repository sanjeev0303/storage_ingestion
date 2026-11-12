"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { MotionPathPlugin } from "gsap/MotionPathPlugin";
import { useRef, type ReactNode } from "react";

gsap.registerPlugin(useGSAP, MotionPathPlugin);

// A satellite riding an orbit inside a decorative SVG. `d` is the orbit as a
// closed path; the children are drawn at the satellite's resting spot, which is
// where it stays without JavaScript or with reduced motion. The loop pauses
// while the drawing is off screen.
export function OrbitSatellite({ d, duration, start = 0, children }: { d: string; duration: number; start?: number; children: ReactNode }) {
  const path = useRef<SVGPathElement>(null);
  const body = useRef<SVGGElement>(null);

  useGSAP(() => {
    const mm = gsap.matchMedia();
    mm.add("(prefers-reduced-motion: no-preference)", () => {
      const tween = gsap.to(body.current, {
        motionPath: { path: path.current!, align: path.current!, alignOrigin: [0.5, 0.5], start, end: start + 1 },
        duration,
        ease: "none",
        repeat: -1,
      });
      const svg = path.current?.ownerSVGElement;
      if (!svg) return;
      const observer = new IntersectionObserver(([entry]) => (entry.isIntersecting ? tween.play() : tween.pause()));
      observer.observe(svg);
      return () => observer.disconnect();
    });
  });

  return (
    <>
      <path ref={path} d={d} fill="none" stroke="none" />
      <g ref={body}>{children}</g>
    </>
  );
}
