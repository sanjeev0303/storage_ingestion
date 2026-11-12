import { cn } from "@/lib/utils/cn";
import "./art.css";
import { OrbitSatellite } from "./orbit-satellite";

// The close's horizon: the orb half-risen behind the far ridge, its orbit
// arcing over the dunes with a satellite riding it, stars in the night sky,
// wind lines on the near slope. Flat layers in the night field's blues; the
// front layer is the field itself, so the art melts into the footer below it.
// Everything drifts slowly (art.css); the satellite sets behind the dunes on
// the lower half of its orbit.

const ORBIT = "M500 300A560 168 0 1 1 1620 300A560 168 0 1 1 500 300";
const STARS = [
  [140, 70, 0],
  [330, 118, 1.4],
  [520, 44, 2.6],
  [760, 96, 0.8],
  [880, 34, 3.1],
  [1240, 136, 1.9],
  [1330, 64, 0.4],
  [1490, 112, 2.2],
];

export function DuneHorizon({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 1600 320" preserveAspectRatio="xMidYMax slice" aria-hidden className={cn("block w-full", className)}>
      {STARS.map(([x, y, delay]) => (
        <circle key={`${x}-${y}`} cx={x} cy={y} r="1.8" fill="#c7cbff" className="art-move art-twinkle" style={{ animationDelay: `${delay}s` }} />
      ))}
      <ellipse cx="1060" cy="300" rx="560" ry="168" fill="none" stroke="#7c84ff" strokeOpacity="0.32" strokeWidth="2" />
      <ellipse
        cx="1060"
        cy="300"
        rx="760"
        ry="236"
        fill="none"
        stroke="#7c84ff"
        strokeOpacity="0.14"
        strokeWidth="1.5"
        strokeDasharray="2 10"
        strokeLinecap="round"
        className="art-dash"
      />
      <g className="art-move art-orb">
        {/* thin rings rather than a filled halo: saffron washed over navy turns brown */}
        <circle cx="1060" cy="196" r="66" fill="none" stroke="#ffc21a" strokeOpacity="0.4" strokeWidth="1.5" className="art-move art-glow" />
        <circle cx="1060" cy="196" r="88" fill="none" stroke="#ffc21a" strokeOpacity="0.16" strokeWidth="1.5" className="art-move art-glow" style={{ animationDelay: "-3s" }} />
        <circle cx="1060" cy="196" r="48" fill="#ffc21a" />
      </g>
      <OrbitSatellite d={ORBIT} duration={30} start={0.12}>
        <circle cx="700" cy="171" r="6" fill="#a3a9ff" />
      </OrbitSatellite>

      <path d="M0 232c180-40 330-30 470-16s290-46 460-26 310 30 470 5 160-10 200-5v130H0z" fill="#161c55" className="art-move art-sway-far" />
      <path d="M0 262c160-26 300-30 430-12s270 20 430-10 290-26 460 4 220 10 280 6v70H0z" fill="#1d2470" className="art-move art-sway-mid" />
      <path d="M0 290c200-25 380-28 560-8s340 18 520-7 300-13 520 10v35H0z" fill="#2a33d6" className="art-move art-sway-near" />
      <g fill="none" stroke="#ffffff" strokeOpacity="0.14" strokeWidth="1.5" strokeLinecap="round">
        <path d="M180 286c60-8 120-9 180-4" className="art-move art-wind" />
        <path d="M640 294c70-6 140-6 210 1" className="art-move art-wind" style={{ animationDelay: "-2.4s" }} />
        <path d="M1180 280c60-7 120-8 190-2" className="art-move art-wind" style={{ animationDelay: "-4.8s" }} />
      </g>
      <path d="M0 312c300-12 600-7 820 2s480 4 780-8v14H0z" fill="var(--night, #0e1130)" />
    </svg>
  );
}
