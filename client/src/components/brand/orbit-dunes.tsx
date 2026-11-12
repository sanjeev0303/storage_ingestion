import { cn } from "@/lib/utils/cn";
import "./art.css";
import { OrbitSatellite } from "./orbit-satellite";

// The sign-in panel's scene, drawn for the ultramarine field: two orbits round
// the risen orb, a satellite riding the inner one, and three translucent white
// dune layers, the nearest the brightest. The orb breathes, the outer orbit's
// dashes travel, the dunes sway and wind lines drift (art.css).

// The inner orbit (centre 392,196, radii 226 by 70, tilted -14 degrees) as one closed path.
const ORBIT = "M172.7 250.7A226 70 -14 1 1 611.3 141.3A226 70 -14 1 1 172.7 250.7";

export function OrbitDunes({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 600 400" preserveAspectRatio="xMidYMax slice" aria-hidden className={cn("block w-full", className)}>
      <ellipse cx="392" cy="196" rx="226" ry="70" transform="rotate(-14 392 196)" fill="none" stroke="#ffffff" strokeOpacity="0.32" strokeWidth="2" />
      <ellipse
        cx="392"
        cy="196"
        rx="300"
        ry="104"
        transform="rotate(-14 392 196)"
        fill="none"
        stroke="#ffffff"
        strokeOpacity="0.14"
        strokeWidth="1.5"
        strokeDasharray="2 9"
        strokeLinecap="round"
        className="art-dash"
      />
      <circle cx="392" cy="182" r="70" fill="#ffc21a" fillOpacity="0.14" className="art-move art-glow" />
      <circle cx="392" cy="182" r="46" fill="#ffc21a" />
      <OrbitSatellite d={ORBIT} duration={22}>
        <circle cx="173" cy="251" r="7" fill="#ffffff" />
        <circle cx="173" cy="251" r="13" fill="none" stroke="#ffffff" strokeOpacity="0.4" strokeWidth="1.5" />
      </OrbitSatellite>

      <path d="M0 268c90-24 170-22 250-6s170-30 260-16 70 12 90 10v144H0z" fill="#ffffff" fillOpacity="0.08" className="art-move art-sway-far" />
      <path d="M0 304c80-16 160-18 240-4s170 14 250-6 80-12 110-8v114H0z" fill="#ffffff" fillOpacity="0.12" className="art-move art-sway-mid" />
      <path d="M0 340c110-14 220-14 320 0s190 6 280-8v68H0z" fill="#ffffff" fillOpacity="0.18" className="art-move art-sway-near" />
      <g fill="none" stroke="#ffffff" strokeOpacity="0.26" strokeWidth="1.5" strokeLinecap="round">
        <path d="M70 352c40-5 80-6 120-2" className="art-move art-wind" />
        <path d="M380 344c40-4 80-5 120-1" className="art-move art-wind" style={{ animationDelay: "-3.5s" }} />
      </g>
    </svg>
  );
}
