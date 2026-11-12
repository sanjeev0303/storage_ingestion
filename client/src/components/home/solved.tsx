import { solved, type Problem as Item } from "./copy";
import "./figures.css";
import { LiveFigure } from "./live-figure";
import { Wrap } from "./wrap";

// Figure 3: four cards, each an 8-second story. The first half plays the
// problem (.c-without), the second half the same moment with Orbidune
// (.c-with). Still, each card shows the fixed state; both sentences above the
// drawing say the whole story without motion.

const ULTRA = "var(--ultra-text)";
const MUTED = "var(--muted)";
const WARN = "var(--warn-mark)";

function Phase({ without }: { without?: boolean }) {
  return (
    <text x="20" y="24" fontSize="12.5" fontWeight="700" style={{ fill: without ? WARN : ULTRA }}>
      {without ? solved.without : solved.with}
    </text>
  );
}

function Cross({ x, y }: { x: number; y: number }) {
  return (
    <g>
      <rect x={x - 7} y={y - 7} width="14" height="14" fill="var(--accent)" stroke="currentColor" strokeWidth="1.4" />
      <path d={`M${x - 3.5} ${y - 3.5}l7 7M${x + 3.5} ${y - 3.5}l-7 7`} stroke="currentColor" strokeWidth="1.5" />
    </g>
  );
}

function FileIcon({ x, y, w = 30, h = 38 }: { x: number; y: number; w?: number; h?: number }) {
  const fold = Math.round(w / 3);
  return (
    <g>
      <path d={`M${x} ${y}h${w - fold}l${fold} ${fold}v${h - fold}H${x}z`} fill="var(--ground)" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
      <path d={`M${x + w - fold} ${y}v${fold}h${fold}`} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
    </g>
  );
}

function Uploads() {
  return (
    <>
      <g className="an c-without">
        <Phase without />
        <rect x="20" y="70" width="260" height="16" rx="8" fill="var(--line)" />
        <rect className="an b-fill" x="20" y="70" width="260" height="16" rx="8" fill={MUTED} />
        <rect className="an b-fail" x="20" y="70" width="156" height="16" rx="8" fill="var(--accent)" />
        <text className="an b-at30" x="20" y="116" fontSize="12.5" style={{ fill: MUTED }}>
          connection lost at 60%
        </text>
        <text className="an b-at40" x="20" y="138" fontSize="12.5" fontWeight="700" style={{ fill: WARN }}>
          starting again from 0%
        </text>
      </g>
      <g className="an c-with">
        <Phase />
        {[0, 1, 2, 3, 4, 5].map((i) => {
          const x = 20 + i * 44;
          const failed = i === 3;
          return (
            <g key={i}>
              <rect x={x} y="60" width="38" height="34" rx="5" fill="var(--ground)" stroke="var(--edge)" strokeWidth="1.2" />
              <rect
                className={failed ? "an b-retry" : "an b-part"}
                x={x}
                y="60"
                width="38"
                height="34"
                rx="5"
                fill={ULTRA}
                style={failed ? undefined : { animationDelay: `${i * 0.32}s` }}
              />
              {failed && (
                <g className="an b-x">
                  <Cross x={x + 19} y={77} />
                </g>
              )}
              <text x={x + 19} y="112" textAnchor="middle" fontSize="11.5" style={{ fill: MUTED }}>
                part {i + 1}
              </text>
            </g>
          );
        })}
        <text className="an b-at82" x="20" y="144" fontSize="12.5" fontWeight="700" style={{ fill: ULTRA }}>
          only part 4 was sent again
        </text>
      </g>
    </>
  );
}

function DeleteChip() {
  return (
    <g>
      <rect x="28" y="72" width="92" height="30" rx="15" fill="currentColor" />
      <text x="74" y="91.5" textAnchor="middle" fontSize="12.5" fontWeight="600" style={{ fill: "var(--ground)" }}>
        Delete
      </text>
    </g>
  );
}

function Holds() {
  return (
    <>
      <g className="an c-without">
        <Phase without />
        <text x="20" y="46" fontSize="12" style={{ fill: MUTED }}>
          hold noted in a spreadsheet
        </text>
        <g className="an b-push">
          <DeleteChip />
        </g>
        <g className="an b-gone">
          <FileIcon x={190} y={62} w={40} h={50} />
        </g>
        <text className="an b-at30" x="20" y="146" fontSize="12.5" fontWeight="700" style={{ fill: WARN }}>
          deleted while on hold
        </text>
      </g>
      <g className="an c-with">
        <Phase />
        <circle cx="210" cy="87" r="40" fill="var(--ultra-soft)" stroke={ULTRA} strokeWidth="2" />
        <FileIcon x={190} y={62} w={40} h={50} />
        <g transform="translate(236 44)">
          <rect width="18" height="14" y="6" rx="3" fill={ULTRA} />
          <path d="M4.5 6V4a4.5 4.5 0 0 1 9 0v2" fill="none" stroke={ULTRA} strokeWidth="2" />
        </g>
        <text x="210" y="146" textAnchor="middle" fontSize="12" fontWeight="600" style={{ fill: ULTRA }}>
          legal hold
        </text>
        <g className="an b-refuse">
          <DeleteChip />
        </g>
        <text className="an b-at70" x="20" y="146" fontSize="12.5" fontWeight="700" style={{ fill: ULTRA }}>
          delete refused
        </text>
      </g>
    </>
  );
}

function Duplicates() {
  return (
    <>
      <FileIcon x={20} y={40} />
      <FileIcon x={20} y={100} />
      <text x="58" y="64" fontSize="12" className="mono">
        a.csv
      </text>
      <text x="58" y="124" fontSize="12" className="mono">
        a-copy.csv
      </text>
      <rect x="196" y="34" width="84" height="104" rx="8" fill="none" stroke="var(--line)" strokeWidth="1.5" />
      <text x="238" y="156" textAnchor="middle" fontSize="11.5" style={{ fill: MUTED }}>
        storage
      </text>
      <g>
        <rect x="210" y="48" width="56" height="26" rx="4" fill="var(--faint)" stroke="currentColor" strokeWidth="1.2" />
        <text x="238" y="65.5" textAnchor="middle" fontSize="11" className="mono">
          9f2c…
        </text>
      </g>
      <g className="an c-without">
        <Phase without />
        <path d="M108 59H204" stroke={MUTED} strokeWidth="1.5" />
        <path d="M148 119H204" stroke={MUTED} strokeWidth="1.5" />
        <g className="an b-drop">
          <rect x="210" y="106" width="56" height="26" rx="4" fill="var(--accent)" stroke="currentColor" strokeWidth="1.2" />
          <text x="238" y="123.5" textAnchor="middle" fontSize="11" className="mono" style={{ fill: "var(--on-accent)" }}>
            9f2c…
          </text>
        </g>
        <text className="an b-at30" x="20" y="162" fontSize="12.5" fontWeight="700" style={{ fill: WARN }}>
          stored twice
        </text>
      </g>
      <g className="an c-with">
        <Phase />
        <path d="M108 59H204" stroke={ULTRA} strokeWidth="1.8" />
        <path className="an b-at70" d="M148 119H180V61H204" fill="none" stroke={ULTRA} strokeWidth="1.8" strokeDasharray="4 3" />
        <text className="an b-at82" x="20" y="162" fontSize="12.5" fontWeight="700" style={{ fill: ULTRA }}>
          same hash, stored once
        </text>
      </g>
    </>
  );
}

const COPIES = [
  { x: 20, label: "bucket" },
  { x: 112, label: "snapshot v1" },
  { x: 204, label: "snapshot v2" },
];

function Erasure() {
  return (
    <>
      {COPIES.map((c) => (
        <g key={c.label}>
          <rect x={c.x} y="54" width="76" height="46" rx="6" fill="var(--ground)" stroke="currentColor" strokeWidth="1.5" />
          <text x={c.x + 38} y="120" textAnchor="middle" fontSize="11.5" style={{ fill: MUTED }}>
            {c.label}
          </text>
        </g>
      ))}
      <g className="an c-without">
        <Phase without />
        <circle className="an b-gone" cx={COPIES[0].x + 38} cy="77" r="9" fill="var(--accent)" stroke="currentColor" strokeWidth="1.2" />
        <circle className="an b-pulse" cx={COPIES[1].x + 38} cy="77" r="9" fill="var(--accent)" stroke="currentColor" strokeWidth="1.2" />
        <circle className="an b-pulse" cx={COPIES[2].x + 38} cy="77" r="9" fill="var(--accent)" stroke="currentColor" strokeWidth="1.2" />
        <text className="an b-at30" x="20" y="148" fontSize="12.5" fontWeight="700" style={{ fill: WARN }}>
          2 copies left in snapshots
        </text>
      </g>
      <g className="an c-with">
        <Phase />
        {COPIES.map((c) => (
          <g key={c.label}>
            <rect className="an b-sweep" x={c.x - 4} y="50" width="84" height="54" rx="8" fill="none" stroke={ULTRA} strokeWidth="3" />
            <circle className="an b-fade70" cx={c.x + 38} cy="77" r="9" fill="var(--accent)" stroke="currentColor" strokeWidth="1.2" />
          </g>
        ))}
        <g className="an b-at82">
          <circle cx="272" cy="20" r="10" fill={ULTRA} />
          <path d="M267 20l3.5 3.5 6-7" fill="none" stroke="var(--ground)" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
          <text x="20" y="148" fontSize="12.5" fontWeight="700" style={{ fill: ULTRA }}>
            every copy erased, certificate signed
          </text>
        </g>
      </g>
    </>
  );
}

const DRAWINGS: Record<string, () => React.ReactNode> = { uploads: Uploads, holds: Holds, duplicates: Duplicates, erasure: Erasure };

function Card({ item, index }: { item: Item; index: number }) {
  const Drawing = DRAWINGS[item.id];
  return (
    <li className="card-anim flex min-w-0 flex-col gap-3 rounded-xl bg-well p-5 ring-1 ring-line lg:p-6">
      <h4 className="text-lg leading-snug font-semibold">
        <span className="mr-1.5 font-wide text-ultra-text">({String.fromCharCode(101 + index)})</span>
        {item.title}
      </h4>
      <p className="t-small text-muted">{item.before}</p>
      <p className="t-small text-ink">{item.after}</p>
      <div className="mt-auto rounded-lg bg-ground p-3 ring-1 ring-line">
        <svg viewBox="0 0 300 170" className="drawing w-full" aria-hidden>
          <Drawing />
        </svg>
      </div>
    </li>
  );
}

export function Solved() {
  return (
    <section id="solved" className="scroll-mt-20 border-t border-line py-24 lg:py-32" aria-labelledby="solved-title">
      <Wrap>
        <div className="grid gap-x-10 gap-y-5 lg:grid-cols-12">
          <h2 id="solved-title" className="t-h2 orbit-stop lg:col-span-6">
            {solved.heading}
          </h2>
          <p className="t-body max-w-[36rem] text-muted lg:col-span-5 lg:col-start-8 lg:self-end">{solved.intro}</p>
        </div>
        <LiveFigure number={solved.number} title={solved.title} className="mt-12">
          <ul className="grid gap-4 p-4 sm:grid-cols-2 sm:p-6 lg:gap-5">
            {solved.items.map((item, i) => (
              <Card key={item.id} item={item} index={i} />
            ))}
          </ul>
        </LiveFigure>
      </Wrap>
    </section>
  );
}
