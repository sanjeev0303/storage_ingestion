import { ArrowDown, ArrowRight, CheckCircle, WarningCircle } from "@phosphor-icons/react/dist/ssr";
import { cn } from "@/lib/utils/cn";
import { problem, type Problem as Item } from "./copy";
import "./figures.css";
import { LiveFigure } from "./live-figure";
import { Wrap } from "./wrap";

type Mode = "before" | "after";

// Each drawing holds both states; the parent's data-mode decides which parts show.
const B = "group-data-[mode=after]:opacity-0";
const A = "opacity-0 group-data-[mode=after]:opacity-100";

const ULTRA = "var(--ultra-text)";
const MUTED = "var(--muted)";

function Arrow({ x1, x2, y, className, color = "currentColor" }: { x1: number; x2: number; y: number; className?: string; color?: string }) {
  return (
    <g className={className} stroke={color} strokeWidth="1.6" fill="none" strokeLinecap="round">
      <path d={`M${x1} ${y}H${x2}`} />
      <path d={`M${x2 - 6} ${y - 5}l6 5-6 5`} strokeLinejoin="round" />
    </g>
  );
}

function Drift() {
  return (
    <>
      {[20, 66, 112].map((x) => (
        <g key={x}>
          <rect x={x} y="52" width="38" height="48" rx="4" fill="var(--ground)" stroke="currentColor" strokeWidth="1.5" />
          <text x={x + 19} y="81" textAnchor="middle" fontSize="12.5" className="mono">
            v1
          </text>
        </g>
      ))}
      <g className={B}>
        {/* a new version drops onto the checked file */}
        <g className="an f2-drop">
          <rect x="66" y="52" width="38" height="48" rx="4" fill="var(--accent)" stroke="currentColor" strokeWidth="1.5" />
          <text x="85" y="81" textAnchor="middle" fontSize="12.5" className="mono" style={{ fill: "var(--on-accent)" }}>
            v2
          </text>
        </g>
        <circle className="an f2-travel" cx="166" cy="76" r="4.5" fill="var(--accent)" stroke="currentColor" strokeWidth="1" style={ {"--dx": "40px"} as React.CSSProperties } />
        <text x="20" y="36" fontSize="12.5" className="an f2-late" style={{ fill: MUTED }}>
          overwritten after review
        </text>
      </g>
      <g className={A}>
        <rect x="11" y="43" width="148" height="66" rx="8" fill="none" stroke={ULTRA} strokeWidth="1.6" />
        <text x="12" y="34" fontSize="12.5" fontWeight="600" style={{ fill: ULTRA }}>
          snapshot pins v1
        </text>
        {/* the new version glances off the pinned snapshot and lands beside it */}
        <rect className="an f2-flash" x="11" y="43" width="148" height="66" rx="8" fill="none" stroke={ULTRA} strokeWidth="4" />
        <g className="an f2-glance">
          <rect x="170" y="108" width="30" height="34" rx="3" fill="var(--ground)" stroke={MUTED} strokeWidth="1.3" />
          <text x="185" y="129" textAnchor="middle" fontSize="12" className="mono" style={{ fill: MUTED }}>
            v2
          </text>
        </g>
        <circle className="an f2-travel" cx="166" cy="76" r="4.5" fill={ULTRA} stroke="currentColor" strokeWidth="1" style={ {"--dx": "40px"} as React.CSSProperties } />
      </g>
      <Arrow x1={166} x2={206} y={76} />
      <rect x="212" y="54" width="58" height="44" rx="6" fill="var(--ground)" stroke="currentColor" strokeWidth="1.5" />
      <text x="241" y="80" textAnchor="middle" fontSize="11.5" fontWeight="600">
        model
      </text>
    </>
  );
}

const BARS = [150, 118, 168, 106, 140];
type Mark = "passed" | "partial" | "none" | "failed";
const AFTER_MARKS: Mark[] = ["passed", "passed", "partial", "none", "failed"];
const AFTER_WORDS = ["passed", "passed", "sampled", "not checked", "failed"];

function MarkShape({ kind, y }: { kind: Mark; y: number }) {
  if (kind === "passed") return <rect x="34" y={y} width="11" height="11" fill={ULTRA} />;
  if (kind === "partial")
    return (
      <g>
        <rect x="34.75" y={y + 0.75} width="9.5" height="9.5" fill="none" stroke={ULTRA} strokeWidth="1.5" />
        <rect x="34" y={y} width="5.5" height="11" fill={ULTRA} />
      </g>
    );
  if (kind === "none")
    return <rect x="34.75" y={y + 0.75} width="9.5" height="9.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeDasharray="2.2 1.8" />;
  return (
    <g>
      <rect x="34.75" y={y + 0.75} width="9.5" height="9.5" fill="var(--accent)" stroke="currentColor" strokeWidth="1.5" />
      <path d={`M37 ${y + 3}l5 5M42 ${y + 3}l-5 5`} stroke="currentColor" strokeWidth="1.4" />
    </g>
  );
}

function Gaps() {
  return (
    <>
      <rect x="18" y="16" width="244" height="124" rx="8" fill="var(--ground)" stroke="var(--line)" strokeWidth="1.5" />
      {/* which panel shows a part (B/A) and how it moves (.an) sit on separate elements,
          because both set opacity */}
      <g className={B}>
        <text x="34" y="38" fontSize="12" fontWeight="600" className="an f2-late">
          Ready
        </text>
      </g>
      <g className={A}>
        <text x="34" y="38" fontSize="12" fontWeight="600" className="an f2-late" textLength="214" lengthAdjust="spacingAndGlyphs">
          1 failed, 1 not checked, 1 sampled
        </text>
      </g>
      {BARS.map((w, i) => {
        const y = 52 + i * 17;
        // the checks tick in one after another
        const beat = { animationDelay: `${i * 0.22}s` };
        return (
          <g key={i}>
            <g className={B}>
              <g className="an f2-pop" style={beat}>
                <MarkShape kind="passed" y={y} />
              </g>
            </g>
            <g className={A}>
              <g className="an f2-pop" style={beat}>
                <MarkShape kind={AFTER_MARKS[i]} y={y} />
              </g>
            </g>
            <rect x="54" y={y + 3} width={w * 0.82} height="5" rx="2.5" fill="var(--line)" />
            <g className={A}>
              <text x="248" y={y + 9.5} textAnchor="end" fontSize="12.5" className="an f2-pop" style={{ fill: MUTED, ...beat }}>
                {AFTER_WORDS[i]}
              </text>
            </g>
          </g>
        );
      })}
    </>
  );
}

function Unsafe() {
  return (
    <>
      <path d="M18 50h32l12 12v42H18z" fill="var(--ground)" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
      <path d="M50 50v12h12" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
      <circle cx="58" cy="96" r="7" fill="var(--accent)" stroke="currentColor" strokeWidth="1.2" />
      <path d="M58 92v4.5M58 99v.5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      <text x="40" y="122" textAnchor="middle" fontSize="12.5" className="mono">
        model.pt
      </text>

      <g className={B}>
        <Arrow x1={70} x2={214} y={77} />
        <text x="142" y="66" textAnchor="middle" fontSize="12.5" style={{ fill: MUTED }}>
          os.system runs on load
        </text>
        {/* the unsafe file travels straight to the reader */}
        <circle className="an f2-travel" cx="70" cy="77" r="4.5" fill="var(--accent)" stroke="currentColor" strokeWidth="1" style={ {"--dx": "144px"} as React.CSSProperties } />
        <circle className="an f2-hit" cx="238" cy="62" r="15" fill="var(--accent)" fillOpacity="0.45" />
      </g>
      <g className={A}>
        <Arrow x1={70} x2={112} y={77} color={ULTRA} />
        <rect x="116" y="40" width="56" height="74" rx="6" fill="var(--ultra-soft)" stroke={ULTRA} strokeWidth="1.6" />
        {/* it stops at the quarantine gate, which shuts on it */}
        <rect className="an f2-hit" x="116" y="40" width="56" height="74" rx="6" fill="none" stroke={ULTRA} strokeWidth="4" />
        <g className="an f2-xpop">
          <rect x="137.75" y="70.75" width="12.5" height="12.5" fill="var(--accent)" stroke="currentColor" strokeWidth="1.4" />
          <path d="M140.5 73.5l7 7M147.5 73.5l-7 7" stroke="currentColor" strokeWidth="1.5" />
        </g>
        <circle className="an f2-travel" cx="70" cy="77" r="4.5" fill={ULTRA} stroke="currentColor" strokeWidth="1" style={ {"--dx": "40px"} as React.CSSProperties } />
        <text x="144" y="132" textAnchor="middle" fontSize="12.5" fontWeight="600" style={{ fill: ULTRA }}>
          blocked in quarantine
        </text>
        <path d="M178 77H214" stroke={MUTED} strokeWidth="1.4" strokeDasharray="3 4" />
      </g>

      <circle cx="238" cy="62" r="9" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <path d="M222 100c2-14 9-22 16-22s14 8 16 22" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      <text x="238" y="122" textAnchor="middle" fontSize="12.5">
        reader
      </text>
    </>
  );
}

function Lineage() {
  const nodes = [
    { x: 14, label: "files", after: "snapshot v3" },
    { x: 108, label: "run", after: "run 18" },
    { x: 202, label: "model", after: "in production" },
  ];
  return (
    <>
      {nodes.map((n) => (
        <g key={n.label}>
          <rect x={n.x} y="56" width="64" height="40" rx="6" fill="var(--ground)" stroke="currentColor" strokeWidth="1.5" />
          <text x={n.x + 32} y="80" textAnchor="middle" fontSize="11.5" fontWeight="600">
            {n.label}
          </text>
          <text x={n.x + 32} y="116" textAnchor="middle" fontSize="12.5" className={A} style={{ fill: ULTRA }}>
            {n.after}
          </text>
        </g>
      ))}
      <g className={B} stroke={MUTED} strokeWidth="1.5" strokeDasharray="3 4">
        <path d="M80 76h26M174 76h26" />
      </g>
      <g className={B}>
        <text x="93" y="66" textAnchor="middle" fontSize="13" fontWeight="700" className="an f2-blink">
          ?
        </text>
        <text x="187" y="66" textAnchor="middle" fontSize="13" fontWeight="700" className="an f2-blink">
          ?
        </text>
      </g>
      <Arrow x1={80} x2={106} y={76} color={ULTRA} className={A} />
      <Arrow x1={174} x2={200} y={76} color={ULTRA} className={A} />
      {/* each link carries the snapshot forward: files to run, run to model */}
      <g className={A}>
        <circle className="an f2-travel" cx="80" cy="76" r="4.5" fill={ULTRA} stroke="currentColor" strokeWidth="1" style={ {"--dx": "26px"} as React.CSSProperties } />
        <circle className="an f2-travel" cx="174" cy="76" r="4.5" fill={ULTRA} stroke="currentColor" strokeWidth="1" style={ {"--dx": "26px", animationDelay: "0.7s"} as React.CSSProperties } />
      </g>
    </>
  );
}

const DRAWINGS: Record<string, () => React.ReactNode> = { drift: Drift, gaps: Gaps, unsafe: Unsafe, lineage: Lineage };

// One side of a comparison: the state's label, its sentence, then the drawing.
function Panel({ item, mode }: { item: Item; mode: Mode }) {
  const Drawing = DRAWINGS[item.id];
  const after = mode === "after";
  const Mark = after ? CheckCircle : WarningCircle;
  return (
    <div
      data-mode={mode}
      className={cn(
        "group flex min-w-0 flex-col gap-3 rounded-xl p-5 ring-1",
        after ? "bg-ultra-soft ring-[color-mix(in_srgb,var(--ultra-text)_35%,transparent)]" : "bg-well ring-line",
      )}
    >
      <p className={cn("inline-flex items-center gap-2 text-sm font-semibold", after ? "text-ultra-text" : "text-ink")}>
        <Mark aria-hidden weight="fill" className={cn("size-[18px] shrink-0", !after && "text-[var(--warn-mark)]")} />
        {problem.modes[mode]}
      </p>
      <p className={cn("t-small", after ? "text-ink" : "text-muted")}>{after ? item.after : item.before}</p>
      <svg viewBox="0 0 280 150" className="drawing mt-auto w-full max-w-[400px] self-center lg:max-w-[330px]" aria-hidden>
        <Drawing />
      </svg>
    </div>
  );
}

export function Problem() {
  return (
    <section id="problem" className="scroll-mt-20 py-24 lg:py-32" aria-labelledby="problem-title">
      <Wrap>
        <div className="grid gap-x-10 gap-y-5 lg:grid-cols-12">
          <h2 id="problem-title" className="t-h2 orbit-stop lg:col-span-6">
            {problem.heading}
          </h2>
          <p className="t-body max-w-[36rem] text-muted lg:col-span-5 lg:col-start-8 lg:self-end">{problem.intro}</p>
        </div>

        <LiveFigure number={problem.number} title={problem.title} className="mt-12">

          <ol>
            {problem.items.map((item, i) => (
              <li
                key={item.id}
                className={cn("grid gap-x-8 gap-y-4 p-5 sm:p-6 lg:grid-cols-12 lg:items-stretch lg:px-8 lg:py-6", i > 0 && "border-t border-line")}
              >
                <h4 className="text-lg leading-snug font-semibold lg:col-span-3 lg:pt-1 lg:pr-2">
                  <span className="mb-1 block font-wide text-ultra-text">({String.fromCharCode(97 + i)})</span>
                  {item.title}
                </h4>
                <div className="grid items-stretch gap-3 sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] sm:gap-4 lg:col-span-9">
                  <Panel item={item} mode="before" />
                  <span aria-hidden className="grid place-items-center">
                    <span className="grid size-10 place-items-center rounded-full bg-ultra text-on-ultra shadow-[0_6px_16px_-6px_rgb(47_59_255/0.6)]">
                      <ArrowRight className="hidden size-5 sm:block" weight="bold" />
                      <ArrowDown className="size-5 sm:hidden" weight="bold" />
                    </span>
                  </span>
                  <Panel item={item} mode="after" />
                </div>
              </li>
            ))}
          </ol>
        </LiveFigure>
      </Wrap>
    </section>
  );
}
