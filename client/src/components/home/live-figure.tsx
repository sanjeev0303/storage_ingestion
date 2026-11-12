"use client";

import { Pause, Play } from "@phosphor-icons/react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { cn } from "@/lib/utils/cn";

// A figure whose drawings loop. The loops run only while the figure is on
// screen and not paused; data-playing drives every animation inside (see
// figures.css). Without JavaScript the attribute is absent and the drawings
// stay still, in their finished state.
export function LiveFigure({
  number,
  title,
  className,
  children,
}: {
  number: string;
  title: string;
  className?: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLElement>(null);
  const [inView, setInView] = useState(false);
  const [paused, setPaused] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const observer = new IntersectionObserver(([entry]) => setInView(entry.isIntersecting), { threshold: 0.15 });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  return (
    <figure ref={ref} className={cn("figure", className)} data-playing={inView && !paused ? "true" : "false"}>
      <figcaption className="figure-head">
        <h3>
          <span className="fig-no">{number}</span> {title}
        </h3>
        <button
          type="button"
          onClick={() => setPaused((p) => !p)}
          aria-pressed={paused}
          className="-my-2 hidden min-h-10 items-center gap-1.5 rounded-full px-3 text-sm font-medium text-muted transition-[color,transform] duration-150 ease-out hover:text-ink active:scale-[0.97] motion-safe:inline-flex"
        >
          {paused ? <Play aria-hidden weight="fill" className="size-4" /> : <Pause aria-hidden weight="fill" className="size-4" />}
          {paused ? "Play animation" : "Pause animation"}
        </button>
      </figcaption>
      {children}
    </figure>
  );
}
