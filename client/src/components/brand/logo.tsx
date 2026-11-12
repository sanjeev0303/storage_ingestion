import { cn } from "@/lib/utils/cn";

// The Orbidune mark: an orbit ring drawn as the O of the name, two dunes
// inside it, and the orb riding the ring as its satellite. The ring and the
// lower dune take the text colour; the upper dune, the orb and the gap round
// the orb read --logo-dune, --logo-orb and --logo-gap, so a coloured field can
// restyle them without a second file.
export function LogoMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 64 64" aria-hidden className={cn("size-7 shrink-0", className)}>
      {/* Every copy of the mark defines the same circle, so one shared id is safe. */}
      <clipPath id="orbidune-mark-o">
        <circle cx="32" cy="32" r="22.5" />
      </clipPath>
      <g clipPath="url(#orbidune-mark-o)">
        <path d="M0 44c9 0 13-10 24-10 9 0 12 8 20 8 6 0 9-3 20-3v25H0z" fill="var(--logo-dune, var(--ultra-text))" />
        <path d="M0 52c10 0 14-6 25-6 10 0 14 5 23 5 7 0 10-2 16-2v15H0z" fill="currentColor" />
      </g>
      <circle cx="32" cy="32" r="25" fill="none" stroke="currentColor" strokeWidth="5" />
      <circle cx="50.5" cy="13.5" r="6.5" fill="var(--logo-orb, #ffc21a)" stroke="var(--logo-gap, var(--ground))" strokeWidth="2.5" />
    </svg>
  );
}

export function Logo({ className, markClassName }: { className?: string; markClassName?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-2.5", className)}>
      <LogoMark className={markClassName} />
      <span translate="no" className="font-wide text-lg leading-none font-bold tracking-[-0.02em]">Orbidune</span>
    </span>
  );
}
