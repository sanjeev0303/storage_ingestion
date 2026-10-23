import type { ReactNode } from "react";
import { cn } from "@/lib/utils/cn";
import type { StopId } from "./copy";

type Tone = "ground" | "plum" | "faint" | "amber";

// ground is the 60%, plum the 30% (and the dark theme's ground), amber the 10%.
const tones: Record<Tone, string> = {
  ground: "bg-ground text-ink",
  plum: "bg-ink text-ground",
  faint: "bg-faint text-ink",
  amber: "bg-accent text-on-accent",
};

export function Container({ children, className }: { children: ReactNode; className?: string }) {
  // The left padding on large screens leaves room for the custody rail.
  return <div className={cn("mx-auto w-full max-w-[var(--content-max)] px-5 sm:px-8 lg:px-[88px]", className)}>{children}</div>;
}

type SectionProps = {
  stop: StopId;
  tone?: Tone;
  // The id of the section's heading, for the landmark name.
  labelledBy: string;
  children: ReactNode;
  className?: string;
  // Sections that hold their own edge-to-edge layout skip the container.
  bleed?: boolean;
};

// One hand-off in the custody record. data-stop is what the rail follows.
export function Section({ stop, tone = "ground", labelledBy, children, className, bleed = false }: SectionProps) {
  return (
    <section
      id={stop}
      data-stop={stop}
      data-tone={tone}
      aria-labelledby={labelledBy}
      className={cn("scroll-mt-0 py-20 lg:py-28", tones[tone], className)}
    >
      {bleed ? children : <Container>{children}</Container>}
    </section>
  );
}
