"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, useState } from "react";
import { cn } from "@/lib/utils/cn";
import { method } from "./copy";
import { Wrap } from "./wrap";

gsap.registerPlugin(useGSAP, ScrollTrigger);

// Eight stations on the upper arc of one ellipse, from Connect on the left to Use on the right.
const CX = 360;
const CY = 360;
const RX = 270;
const RY = 250;
const START = 168;
const SPAN = 156;
const COUNT = method.stages.length;

function at(i: number, grow = 0) {
  const a = ((START - (i * SPAN) / (COUNT - 1)) * Math.PI) / 180;
  return { x: CX + (RX + grow) * Math.cos(a), y: CY - (RY + grow) * Math.sin(a), cos: Math.cos(a) };
}

const first = at(0);
const last = at(COUNT - 1);
const ARC = `M${first.x.toFixed(1)} ${first.y.toFixed(1)}A${RX} ${RY} 0 0 1 ${last.x.toFixed(1)} ${last.y.toFixed(1)}`;
const RAIL_X1 = 210;
const RAIL_X2 = 530;
const RAIL_Y = [262, 296, 330];

export function Method() {
  const section = useRef<HTMLElement>(null);
  const drawn = useRef<SVGPathElement>(null);
  const pulses = useRef<(SVGCircleElement | null)[]>([]);
  const trigger = useRef<ScrollTrigger | null>(null);
  const [active, setActive] = useState(0);
  // Pinned and scroll-driven on wide screens with motion; otherwise every stage shows at once.
  const [pinned, setPinned] = useState(false);

  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add("(min-width: 1024px) and (prefers-reduced-motion: no-preference)", () => {
        setPinned(true);
        gsap.set(drawn.current, { strokeDashoffset: 1 });
        trigger.current = ScrollTrigger.create({
          trigger: section.current,
          start: "top top",
          end: "bottom bottom",
          scrub: 0.4,
          onUpdate: (self) => {
            const p = self.progress;
            gsap.set(drawn.current, { strokeDashoffset: 1 - p });
            gsap.set(pulses.current, { attr: { cx: RAIL_X1 + p * (RAIL_X2 - RAIL_X1) } });
            setActive(Math.min(COUNT - 1, Math.floor(p * COUNT)));
          },
        });
        return () => {
          setPinned(false);
          trigger.current = null;
        };
      });
    },
    { scope: section },
  );

  // A stage button scrolls the pinned figure to that stage; elsewhere it just selects it.
  function show(i: number) {
    const st = trigger.current;
    if (!st) {
      setActive(i);
      return;
    }
    const target = st.start + ((i + 0.5) / COUNT) * (st.end - st.start);
    window.scrollTo({ top: target, behavior: "smooth" });
  }

  const stage = method.stages[active];

  return (
    // Tall and sticky only while scroll drives it; otherwise an ordinary section.
    <section id="method" ref={section} className={cn("relative scroll-mt-20 border-t border-line", pinned && "h-[230svh]")} aria-labelledby="method-title">
      <div className={cn("py-24 lg:py-32", pinned && "sticky top-0 flex h-svh items-center pt-20 pb-0 lg:pt-20 lg:pb-0")}>
        <Wrap className={cn("grid gap-x-10 gap-y-10 lg:grid-cols-12", pinned ? "lg:items-center" : "lg:items-start")}>
          <div className="lg:col-span-5">
            <h2 id="method-title" className="t-h2 orbit-stop">
              {method.heading}
            </h2>
            <p className="t-body mt-5 max-w-[34rem] text-muted">{method.intro}</p>

            <ol className="mt-8 border-t border-line">
              {method.stages.map((s, i) => {
                const open = !pinned || i === active;
                return (
                  <li key={s.id} className="border-b border-line">
                    <button
                      type="button"
                      onClick={() => show(i)}
                      aria-current={pinned && i === active ? "step" : undefined}
                      className="flex min-h-11 w-full items-baseline gap-3 py-2.5 text-left"
                    >
                      <span className={cn("w-5 font-wide text-sm font-bold tabular-nums", i <= active && pinned ? "text-ultra-text" : "text-muted")}>
                        {i + 1}
                      </span>
                      <span className={cn("text-[1.0625rem] font-semibold transition-colors duration-200", pinned && i !== active && "text-muted")}>
                        {s.name}
                      </span>
                      <span className="text-sm text-muted">{s.sub}</span>
                    </button>
                    <div
                      className={cn(
                        "grid transition-[grid-template-rows,opacity] duration-300 ease-out",
                        open ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0",
                      )}
                    >
                      <div className="overflow-hidden" inert={!open || undefined}>
                        <p className="pb-2 pl-8 t-small">{s.text}</p>
                        <ul className="flex flex-wrap gap-1.5 pb-3.5 pl-8">
                          {s.features.map((f) => (
                            <li key={f} className="rounded-md bg-ground px-2.5 py-1 text-[0.8125rem] text-muted ring-1 ring-line">
                              {f}
                            </li>
                          ))}
                        </ul>
                      </div>
                    </div>
                  </li>
                );
              })}
            </ol>
          </div>

          <figure className={cn("figure lg:col-span-7", !pinned && "lg:sticky lg:top-8")}>
            <figcaption className="figure-head">
              <h3>
                <span className="fig-no">{method.number}</span> {method.title}
              </h3>
            </figcaption>
            <svg viewBox="0 0 720 400" className="drawing block w-full" aria-hidden>
              <path d={ARC} fill="none" stroke="var(--line)" strokeWidth="2" strokeDasharray="2 6" strokeLinecap="round" />
              <path
                ref={drawn}
                d={ARC}
                pathLength={1}
                strokeDasharray="1"
                fill="none"
                stroke="var(--ultra-text)"
                strokeWidth="3"
                strokeLinecap="round"
              />

              {method.stages.map((s, i) => {
                const p = at(i);
                const l = at(i, 30);
                const anchor = p.cos < -0.35 ? "end" : p.cos > 0.35 ? "start" : "middle";
                const isActive = pinned && i === active;
                const done = !pinned || i < active;
                return (
                  <g key={s.id}>
                    {isActive && <circle cx={p.x} cy={p.y} r="18" fill="none" stroke="var(--ultra-text)" strokeWidth="1.5" className="orb-ping" />}
                    <circle
                      cx={p.x}
                      cy={p.y}
                      r={isActive ? 11 : 8}
                      className="transition-[fill,stroke] duration-300"
                      fill={isActive ? "var(--accent)" : done ? "var(--ultra-text)" : "var(--ground)"}
                      stroke={isActive ? "var(--ink)" : done ? "var(--ultra-text)" : "var(--edge)"}
                      strokeWidth="2"
                    />
                    {/* On a phone the names are too small to read, so stations carry their list number. */}
                    <g className="fig3-num">
                      <circle cx={p.x} cy={p.y} r="17" fill="var(--ultra-text)" />
                      <text x={p.x} y={p.y + 7} textAnchor="middle" fontSize="20" fontWeight="700" style={{ fill: "var(--ground)" }}>
                        {i + 1}
                      </text>
                    </g>
                    <text x={l.x} y={l.y + 5} textAnchor={anchor} fontSize="15" className="fig3-label" fontWeight={isActive ? 700 : 600} style={{ fill: isActive || !pinned ? "var(--ink)" : "var(--muted)" }}>
                      {s.name}
                    </text>
                  </g>
                );
              })}

              <text x={CX} y="186" textAnchor="middle" fontSize="30" fontWeight="700" className="font-wide fig3-title" style={{ fontStretch: "125%" }}>
                {pinned ? stage.name : "8 stages"}
              </text>
              <text x={CX} y="212" textAnchor="middle" fontSize="14" style={{ fill: "var(--muted)" }}>
                {pinned ? stage.sub : "from storage to a trained model"}
              </text>

              {method.rails.map((rail, i) => (
                <g key={rail}>
                  <text x={RAIL_X1 - 14} y={RAIL_Y[i] + 4.5} textAnchor="end" fontSize="12.5" className="fig3-rail" style={{ fill: "var(--muted)" }}>
                    {rail}
                  </text>
                  <path d={`M${RAIL_X1} ${RAIL_Y[i]}H${RAIL_X2}`} stroke="var(--line)" strokeWidth="6" strokeLinecap="round" />
                  <circle
                    ref={(el) => void (pulses.current[i] = el)}
                    cx={pinned ? RAIL_X1 : RAIL_X2}
                    cy={RAIL_Y[i]}
                    r="6"
                    fill="var(--ultra-text)"
                  />
                </g>
              ))}
            </svg>
          </figure>
        </Wrap>
      </div>
    </section>
  );
}
