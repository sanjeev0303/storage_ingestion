"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { useId, useRef, useState, type KeyboardEvent, type RefObject } from "react";
import { cn } from "@/lib/utils/cn";

gsap.registerPlugin(useGSAP);

// The pieces the interactive flow diagrams share. A box's border says
// whether the chosen path reached it: solid when it did, dashed when it did
// not (the same dashed square the page uses for "not checked"), amber when
// it is where the path ends. Boxes fill with --box and labels sit on --mask,
// which the section sets for its field.

export type BoxState = "on" | "off" | "end";

type BoxProps = {
  x: number;
  y: number;
  w: number;
  h: number;
  title: string;
  sub?: string;
  state?: BoxState;
};

export function Box({ x, y, w, h, title, sub, state = "on" }: BoxProps) {
  const end = state === "end";
  // Amber is a fill with plum on it, whatever the section's ink.
  const ink = end ? { fill: "var(--box-end-ink, var(--on-accent))" } : undefined;
  return (
    <g data-box data-state={state}>
      <rect
        x={x}
        y={y}
        width={w}
        height={h}
        fill={end ? "var(--box-end, var(--accent))" : "var(--box)"}
        stroke={end ? "var(--box-end-ink, var(--on-accent))" : "currentColor"}
        strokeWidth={state === "off" ? 1.5 : 2}
        strokeDasharray={state === "off" ? "5 4" : undefined}
      />
      <text x={x + w / 2} y={sub ? y + h / 2 - 3 : y + h / 2 + 5} textAnchor="middle" fontSize="15" fontWeight="600" style={ink}>
        {title}
      </text>
      {sub && (
        <text x={x + w / 2} y={y + h / 2 + 16} textAnchor="middle" fontSize="13" style={ink}>
          {sub}
        </text>
      )}
    </g>
  );
}

type Point = readonly [number, number];

// An orthogonal connector through the points, its corners rounded, with an
// arrowhead at the last point. Dashed when the chosen path does not use it.
export function Wire({
  points: given,
  on = true,
  label,
}: {
  points: readonly Point[];
  on?: boolean;
  label?: { x: number; y: number; text: string };
}) {
  const r = 8;
  // Repeated points and points in the middle of a straight run are not corners.
  const points = given.filter((p, i) => {
    if (i === 0) return true;
    const prev = given[i - 1];
    if (p[0] === prev[0] && p[1] === prev[1]) return false;
    const next = given[i + 1];
    return !next || !((prev[0] === p[0] && p[0] === next[0]) || (prev[1] === p[1] && p[1] === next[1]));
  });
  let d = `M${points[0][0]} ${points[0][1]}`;
  for (let i = 1; i < points.length - 1; i++) {
    const [px, py] = points[i - 1];
    const [cx, cy] = points[i];
    const [nx, ny] = points[i + 1];
    const inX = Math.sign(cx - px) * Math.min(r, Math.abs(cx - px) / 2);
    const inY = Math.sign(cy - py) * Math.min(r, Math.abs(cy - py) / 2);
    const outX = Math.sign(nx - cx) * Math.min(r, Math.abs(nx - cx) / 2);
    const outY = Math.sign(ny - cy) * Math.min(r, Math.abs(ny - cy) / 2);
    d += ` L${cx - inX} ${cy - inY} Q${cx} ${cy} ${cx + outX} ${cy + outY}`;
  }
  const [ex, ey] = points[points.length - 1];
  const [bx, by] = points[points.length - 2];
  d += ` L${ex} ${ey}`;
  const dx = Math.sign(ex - bx);
  const dy = Math.sign(ey - by);
  // The head's two sides, 8px back along the last segment and 6px out.
  const head = `M${ex - dx * 8 - dy * 6} ${ey - dy * 8 + dx * 6} L${ex} ${ey} L${ex - dx * 8 + dy * 6} ${ey - dy * 8 - dx * 6}`;
  return (
    <g data-wire data-on={on} stroke="currentColor" strokeWidth={on ? 2 : 1.5} fill="none" strokeDasharray={on ? undefined : "5 4"}>
      <path d={d} />
      <path d={head} strokeDasharray="none" />
      {label && (
        <g stroke="none">
          <rect
            x={label.x - label.text.length * 3.6 - 6}
            y={label.y - 13}
            width={label.text.length * 7.2 + 12}
            height={18}
            fill="var(--mask)"
          />
          <text x={label.x} y={label.y} textAnchor="middle" fontSize="13">
            {label.text}
          </text>
        </g>
      )}
    </g>
  );
}

// Moves the amber token along the chosen path whenever the choice changes.
// The token is drawn where the path ends, so without script, with reduced
// motion, or after a keyboard choice, the picture is already right.
export function useTokenRun(scope: RefObject<Element | null>, path: readonly Point[], key: string, animateRef: RefObject<boolean>) {
  useGSAP(
    () => {
      if (!animateRef.current) return;
      animateRef.current = false;
      const token = scope.current?.querySelector("[data-token]");
      if (!token || path.length < 2) return;
      const [fx, fy] = path[path.length - 1];
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        // About a second whatever the path's length, so the text never waits on it.
        const step = Math.min(0.28, 0.9 / (path.length - 1));
        const tl = gsap.timeline({ defaults: { ease: "power2.inOut" } });
        tl.set(token, { x: path[0][0] - fx, y: path[0][1] - fy });
        path.slice(1).forEach(([x, y]) => {
          tl.to(token, { x: x - fx, y: y - fy, duration: step });
        });
        // The boxes the path reaches settle in after it.
        tl.fromTo(
          scope.current?.querySelectorAll("[data-box][data-state='end']") ?? [],
          { scale: 0.96, transformOrigin: "center", transformBox: "fill-box" },
          { scale: 1, duration: 0.2, ease: "power2.out" },
          "-=0.1",
        );
      });
      return () => mm.revert();
    },
    { scope, dependencies: [key] },
  );
}

type Option<T extends string> = { id: T; label: string };

// A row of choices, as the hero's report uses: a pointer choice animates, an
// arrow-key choice does not (it would only slow the keyboard down).
export function useChoice<T extends string>(first: T) {
  const [value, setValue] = useState<T>(first);
  const animate = useRef(false);
  const arrowKey = useRef(false);
  return {
    value,
    animate,
    choose(id: T) {
      animate.current = !arrowKey.current;
      arrowKey.current = false;
      setValue(id);
    },
    onKeyDown(event: KeyboardEvent) {
      if (event.key.startsWith("Arrow")) arrowKey.current = true;
    },
    onPointerDown() {
      arrowKey.current = false;
    },
    // For another control on the same diagram, such as a switch.
    animateNext() {
      animate.current = true;
    },
  };
}

type ChoicesProps<T extends string> = {
  legend: string;
  options: readonly Option<T>[];
  value: T;
  onChoose: (id: T) => void;
  onKeyDown: (event: KeyboardEvent) => void;
  onPointerDown: () => void;
  className?: string;
};

export function Choices<T extends string>({ legend, options, value, onChoose, onKeyDown, onPointerDown, className }: ChoicesProps<T>) {
  const name = useId();
  return (
    <fieldset className={cn("min-w-0 border-0 p-0", className)} onKeyDown={onKeyDown} onPointerDown={onPointerDown}>
      <legend className="t-small mb-2 p-0 font-medium">{legend}</legend>
      <div className="flex flex-wrap gap-2">
        {options.map((o) => (
          <label key={o.id} className="choice t-small">
            <input type="radio" name={name} value={o.id} checked={o.id === value} onChange={() => onChoose(o.id)} className="sr-only" />
            {o.label}
          </label>
        ))}
      </div>
    </fieldset>
  );
}

// Two short notes that sit beside or under a diagram.
export function Notes({ notes, className }: { notes: readonly { head: string; text: string }[]; className?: string }) {
  return (
    <div className={cn("grid gap-8 sm:grid-cols-2", className)}>
      {notes.map((n) => (
        <p key={n.head} className="t-body max-w-[46ch]">
          <span className="font-semibold">{n.head}</span> {n.text}
        </p>
      ))}
    </div>
  );
}

export type Step = { label: string; note?: string; state: BoxState };

// The phone's version of a flow: the same path as a list, each step marked
// the way its box is drawn (filled when reached, dashed when not, amber where
// the path ends). Screen readers get it as a real list on every width.
export function StepList({ steps, className }: { steps: readonly Step[]; className?: string }) {
  return (
    <ol className={cn("grid gap-2 md:hidden", className)}>
      {steps.map((s, i) => (
        <li
          key={i}
          className={cn(
            "flex items-start gap-3 p-3",
            s.state === "end" && "bg-[var(--box-end,var(--accent))] text-[var(--box-end-ink,var(--on-accent))]",
            s.state === "on" && "bg-[var(--box)] outline-2 -outline-offset-2 outline-[var(--fg)]",
            s.state === "off" && "outline-[1.5px] -outline-offset-2 outline-dashed outline-[var(--fg)]",
          )}
        >
          <svg viewBox="0 0 16 16" className="mt-1 size-3.5 shrink-0" aria-hidden>
            <rect
              x="1.5"
              y="1.5"
              width="13"
              height="13"
              fill={s.state === "off" ? "none" : "currentColor"}
              stroke="currentColor"
              strokeWidth="1.5"
              strokeDasharray={s.state === "off" ? "2.5 2" : undefined}
            />
          </svg>
          <span className="t-body min-w-0 break-words">
            <span className="font-semibold">{s.label}</span>
            {s.note && <span className="block t-small">{s.note}</span>}
            {s.state === "off" && <span className="sr-only"> (not reached)</span>}
            {s.state === "end" && <span className="sr-only"> (where it ends)</span>}
          </span>
        </li>
      ))}
    </ol>
  );
}

// An on/off switch beside a diagram: a box that is ticked, so it never looks
// like a chosen option or a button.
export function Switch({ label, checked, onChange }: { label: string; checked: boolean; onChange: (checked: boolean) => void }) {
  return (
    <label className="choice switch t-small">
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} className="sr-only" />
      <svg viewBox="0 0 16 16" className="size-4 shrink-0" aria-hidden>
        <rect x="1.5" y="1.5" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.5" />
        {checked && <path d="M4.5 8.5l2.5 2.5 4.5-5.5" fill="none" stroke="currentColor" strokeWidth="1.8" />}
      </svg>
      {label}
    </label>
  );
}
