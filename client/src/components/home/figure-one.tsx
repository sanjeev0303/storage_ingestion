"use client";

import { ArrowCounterClockwise } from "@phosphor-icons/react";
import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { MotionPathPlugin } from "gsap/MotionPathPlugin";
import { useRef, useState } from "react";
import { cn } from "@/lib/utils/cn";
import { figure1 } from "./copy";

gsap.registerPlugin(useGSAP, MotionPathPlugin);

// Six stations on one orbit. Each segment is its own path so the orb can ride
// it and the drawn line can follow it; together they read as one stroke.
const POINTS = [
  [56, 262],
  [130, 150],
  [240, 98],
  [360, 96],
  [462, 146],
  [508, 262],
] as const;

const SEGMENTS = [
  "M56 262C60 210 90 168 130 150",
  "M130 150C165 128 200 104 240 98",
  "M240 98C280 92 320 90 360 96",
  "M360 96C400 102 438 120 462 146",
  "M462 146C486 172 504 214 508 262",
];

const ORBIT = "M56 262C60 210 90 168 130 150C165 128 200 104 240 98C280 92 320 90 360 96C400 102 438 120 462 146C486 172 504 214 508 262";

// Where each station's name and note sit, so neither crosses the line.
function labelFor(i: number) {
  const [x, y] = POINTS[i];
  if (i === 0) return { x: x + 18, y: y - 4, noteY: y + 14, anchor: "start" as const };
  if (i === POINTS.length - 1) return { x: x - 18, y: y - 4, noteY: y + 14, anchor: "end" as const };
  // Notes for the side stations sit inside the arc, clear of the line.
  if (i === 1) return { x, y: y - 20, noteX: x + 10, noteY: y + 30, anchor: "middle" as const, noteAnchor: "start" as const };
  if (i === 4) return { x, y: y - 20, noteX: x - 10, noteY: y + 30, anchor: "middle" as const, noteAnchor: "end" as const };
  return { x, y: y - 20, noteY: y + 30, anchor: "middle" as const };
}

export function FigureOne() {
  const root = useRef<HTMLElement>(null);
  const orb = useRef<SVGCircleElement>(null);
  const segments = useRef<(SVGPathElement | null)[]>([]);
  const drawn = useRef<(SVGPathElement | null)[]>([]);
  const timeline = useRef<gsap.core.Timeline | null>(null);
  // -1 before the file arrives; 0..5 the station it is at.
  const [stage, setStage] = useState(-1);
  const [sealed, setSealed] = useState(false);

  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        gsap.set(orb.current, { x: POINTS[0][0], y: POINTS[0][1], opacity: 0 });
        gsap.set(drawn.current, { strokeDashoffset: 1 });
        const tl = gsap.timeline({ delay: 0.3 });
        tl.to(orb.current, { opacity: 1, duration: 0.2 });
        tl.call(() => setStage(0));
        SEGMENTS.forEach((_, i) => {
          const path = segments.current[i];
          tl.to(
            orb.current,
            { duration: 0.55, ease: "power1.inOut", motionPath: { path: path!, align: path!, alignOrigin: [0.5, 0.5] } },
            ">0.15",
          );
          tl.to(drawn.current[i], { strokeDashoffset: 0, duration: 0.55, ease: "power1.inOut" }, "<");
          tl.call(() => setStage(i + 1));
        });
        tl.call(() => setSealed(true), [], ">0.2");
        tl.to(orb.current, { opacity: 0, scale: 0.6, transformOrigin: "50% 50%", duration: 0.3 }, "<");
        timeline.current = tl;
      });
      // Reduced motion: the finished figure, nothing moving.
      mm.add("(prefers-reduced-motion: reduce)", () => {
        gsap.set(orb.current, { opacity: 0 });
        gsap.set(drawn.current, { strokeDashoffset: 0 });
        setStage(POINTS.length - 1);
        setSealed(true);
      });
    },
    { scope: root },
  );

  // The timeline was made inside the hook's context, so restarting it here is safe.
  function replay() {
    const tl = timeline.current;
    if (!tl) return;
    setSealed(false);
    setStage(-1);
    gsap.set(drawn.current, { strokeDashoffset: 1 });
    tl.restart(true);
  }

  const current = stage >= 0 ? figure1.stations[stage] : null;

  return (
    <figure ref={root} className="figure overflow-hidden shadow-[0_24px_60px_-28px_rgb(8_10_40/0.55)]" data-sealed={sealed || undefined}>
      <figcaption className="figure-head">
        <h2>
          <span className="fig-no">{figure1.number}</span> {figure1.title}
        </h2>
        <button
          type="button"
          onClick={replay}
          disabled={!sealed}
          className="-my-2 inline-flex min-h-10 items-center gap-1.5 rounded-full px-3 text-sm font-medium text-muted transition-[color,transform] duration-150 ease-out hover:text-ink active:scale-[0.97] disabled:opacity-0"
        >
          <ArrowCounterClockwise aria-hidden className="size-4" />
          <span className="max-sm:sr-only">{figure1.replay}</span>
        </button>
      </figcaption>

      <svg viewBox="0 56 560 274" className="drawing block w-full" aria-hidden>
        {/* the dune the orbit rises over */}
        <path d="M0 330V300c110-34 190 8 290-10s170-14 270-6v46z" fill="var(--ultra-soft)" />
        <path d={ORBIT} fill="none" stroke="var(--line)" strokeWidth="2" strokeDasharray="2 6" strokeLinecap="round" />
        {SEGMENTS.map((d, i) => (
          <path key={`s${i}`} ref={(el) => void (segments.current[i] = el)} d={d} fill="none" stroke="none" />
        ))}
        {SEGMENTS.map((d, i) => (
          <path
            key={`d${i}`}
            ref={(el) => void (drawn.current[i] = el)}
            d={d}
            pathLength={1}
            strokeDasharray="1"
            fill="none"
            stroke="var(--ultra-text)"
            strokeWidth="2.5"
            strokeLinecap="round"
          />
        ))}

        {figure1.stations.map((station, i) => {
          const [x, y] = POINTS[i];
          const label = labelFor(i);
          const done = sealed || i < stage;
          const active = !sealed && i === stage;
          const isSnapshot = i === POINTS.length - 1;
          return (
            <g key={station.id}>
              <circle
                cx={x}
                cy={y}
                r="9"
                className="transition-[fill,stroke] duration-300"
                fill={isSnapshot && sealed ? "var(--accent)" : done ? "var(--ultra-text)" : "var(--ground)"}
                stroke={active || done ? "var(--ultra-text)" : "var(--edge)"}
                strokeWidth="2"
              />
              {active && (
                <circle cx={x} cy={y} r="15" fill="none" stroke="var(--ultra-text)" strokeWidth="1.5" className="orb-ping" />
              )}
              {done && !(isSnapshot && sealed) && (
                <path d={`M${x - 4} ${y}l3 3 5-6`} fill="none" stroke="var(--ground)" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
              )}
              {isSnapshot && sealed && (
                <g transform={`translate(${x - 5} ${y - 6})`} fill="var(--on-accent)">
                  <rect x="0" y="5" width="10" height="7" rx="1.5" />
                  <path d="M2.2 5V3.6a2.8 2.8 0 0 1 5.6 0V5" fill="none" stroke="var(--on-accent)" strokeWidth="1.6" />
                </g>
              )}
              <text x={label.x} y={label.y} textAnchor={label.anchor} fontSize="14" fontWeight="600" className="fig1-name">
                {station.name}
              </text>
              <text x={label.noteX ?? label.x} y={label.noteY} textAnchor={label.noteAnchor ?? label.anchor} fontSize="12.5" className="fig1-note" style={{ fill: "var(--muted)" }}>
                {station.note}
              </text>
            </g>
          );
        })}

        {/* the file card inside the orbit */}
        <g transform="translate(176 196)" className="fig1-card">
          <rect width="208" height="70" rx="8" fill="var(--ground)" stroke="var(--line)" />
          <text x="14" y="24" fontSize="12.5" className="mono" fontWeight="500">
            {figure1.file}
          </text>
          <text x="14" y="42" fontSize="11" className="mono" style={{ fill: "var(--muted)" }}>
            {figure1.hash}
          </text>
          <text x="14" y="60" fontSize="11.5" fontWeight="600" style={{ fill: "var(--ultra-text)" }}>
            {sealed ? "Published in snapshot v3" : current ? current.note : "Waiting for upload"}
          </text>
        </g>

        <circle ref={orb} r="7" fill="var(--accent)" stroke="var(--ink)" strokeWidth="1.5" />
      </svg>

      <p className={cn("border-t border-line px-[18px] py-3 text-sm transition-opacity duration-300", sealed ? "opacity-100" : "opacity-0")} aria-hidden={!sealed}>
        {figure1.sealed}
      </p>

      <ol className="sr-only">
        {figure1.stations.map((s) => (
          <li key={s.id}>
            {s.name}: {s.note}
          </li>
        ))}
        <li>{figure1.sealed}</li>
      </ol>
    </figure>
  );
}
