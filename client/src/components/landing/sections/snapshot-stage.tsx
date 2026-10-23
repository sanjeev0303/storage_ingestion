"use client";

import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils/cn";
import { copy } from "../copy";
import { usePrefersReducedMotion } from "../diagram-kit";
import { SnapshotDraft } from "../diagrams/snapshot-draft";

gsap.registerPlugin(ScrollTrigger);

const stages = copy.snapshot.stages;
const last = stages.length - 1;
const notchAt = (index: number) => `calc(22px + (100% - 44px) * ${index} / ${last})`;

const sidePadding = "px-5 sm:px-8 lg:pr-12 lg:pl-[max(88px,calc((100vw-var(--content-max))/2+88px))]";

export function SnapshotStage() {
  const [stage, setStage] = useState(0);
  const [playing, setPlaying] = useState(false);
  const timeline = useRef<gsap.core.Timeline | null>(null);
  const clause = useRef<HTMLParagraphElement>(null);
  const root = useRef<HTMLDivElement>(null);
  // Scrolling steps through the stages until the reader takes the slider or presses Play.
  const manual = useRef(false);
  const timers = useRef<number[]>([]);
  const reduced = usePrefersReducedMotion();

  // The slider scrubs the diagram's timeline; it eases between notches unless motion is reduced.
  useEffect(() => {
    const tl = timeline.current;
    if (!tl) return;
    const progress = stage / last;
    if (reduced) {
      tl.progress(progress);
      return;
    }
    const tween = gsap.to(tl, { progress, duration: 0.5, ease: "power2.out" });
    const reveal = clause.current
      ? gsap.fromTo(clause.current, { autoAlpha: 0, y: 12 }, { autoAlpha: 1, y: 0, duration: 0.25, ease: "power2.out" })
      : null;
    return () => {
      tween.kill();
      reveal?.kill();
    };
  }, [stage, reduced]);

  useEffect(() => {
    if (reduced) return;
    const trigger = ScrollTrigger.create({
      trigger: root.current,
      start: "top 60%",
      end: "bottom 55%",
      onUpdate: (self) => {
        if (!manual.current) setStage(Math.round(self.progress * last));
      },
    });
    return () => trigger.kill();
  }, [reduced]);

  useEffect(() => () => timers.current.forEach(window.clearTimeout), []);

  function stopPlaying() {
    timers.current.forEach(window.clearTimeout);
    timers.current = [];
    setPlaying(false);
  }

  function play() {
    manual.current = true;
    stopPlaying();
    setPlaying(true);
    setStage(0);
    for (let step = 1; step <= last; step++) {
      timers.current.push(window.setTimeout(() => setStage(step), step * 1800));
    }
    timers.current.push(window.setTimeout(() => setPlaying(false), last * 1800 + 600));
  }

  return (
    <div ref={root} className="grid lg:min-h-[88svh] lg:grid-cols-2 lg:grid-rows-[auto_auto_1fr_auto]">
      <h2
        id="snapshot-title"
        className={cn("t-h2 order-1 pt-16 lg:col-start-1 lg:row-start-1 lg:pt-28", sidePadding)}
      >
        {copy.snapshot.title}
      </h2>

      <div
        className={cn(
          "order-2 mt-8 overflow-hidden bg-faint pt-4 pb-9 text-ink lg:col-start-2 lg:row-span-4 lg:row-start-1 lg:mt-0 lg:grid lg:place-items-center lg:overflow-visible lg:px-12 lg:py-16",
        )}
      >
        <div className="mx-auto h-[calc(46svh-2rem)] w-full max-w-md overflow-hidden px-5 lg:h-auto lg:max-w-[34rem] lg:overflow-visible lg:px-0">
          <SnapshotDraft timelineRef={timeline} />
        </div>
      </div>

      <div className={cn("relative z-10 order-3 -mt-[22px] pb-6 lg:col-start-1 lg:row-start-4 lg:mt-0 lg:pb-16", sidePadding)}>
        <div className="flex flex-col gap-5 sm:flex-row sm:items-start sm:gap-8">
          <div className="min-w-0 flex-1">
            <div className="relative h-11">
              <div aria-hidden className="absolute inset-x-[22px] top-1/2 h-0.5 -translate-y-1/2 bg-ink" />
              {stages.map((item, index) => (
                <span
                  key={item.name}
                  aria-hidden
                  style={{ left: notchAt(index) }}
                  className="absolute top-1/2 size-3 -translate-x-1/2 -translate-y-1/2 border-2 border-ink bg-ground"
                />
              ))}
              <input
                type="range"
                min={0}
                max={last}
                step={1}
                value={stage}
                aria-label={copy.snapshot.sliderLabel}
                aria-valuetext={stages[stage].name}
                onChange={(event) => {
                  manual.current = true;
                  stopPlaying();
                  setStage(Number(event.target.value));
                }}
                data-taste-skill-ignore className="absolute inset-0 h-11 w-full cursor-pointer focus-visible:outline-2 focus-visible:outline-offset-2 appearance-none bg-transparent [&::-moz-range-thumb]:size-11 [&::-moz-range-thumb]:cursor-grab [&::-moz-range-thumb]:rounded-none [&::-moz-range-thumb]:border-0 [&::-moz-range-thumb]:bg-ink [&::-moz-range-track]:bg-transparent [&::-webkit-slider-runnable-track]:h-11 [&::-webkit-slider-runnable-track]:bg-transparent [&::-webkit-slider-thumb]:size-11 [&::-webkit-slider-thumb]:cursor-grab [&::-webkit-slider-thumb]:appearance-none [&::-webkit-slider-thumb]:rounded-none [&::-webkit-slider-thumb]:border-0 [&::-webkit-slider-thumb]:bg-ink focus-visible:[&::-moz-range-thumb]:bg-accent focus-visible:[&::-webkit-slider-thumb]:bg-accent"
              />
            </div>
            <div aria-hidden className="relative mt-2 h-6">
              {stages.map((item, index) => (
                <span
                  key={item.name}
                  style={index === 0 ? { left: 0 } : index === last ? { right: 0 } : { left: notchAt(index) }}
                  className={cn(
                    "t-small absolute top-0 whitespace-nowrap",
                    index !== 0 && index !== last && "-translate-x-1/2",
                    index === stage ? "font-semibold text-ink" : "text-muted",
                  )}
                >
                  {item.name}
                </span>
              ))}
            </div>
          </div>
          <button
            type="button"
            onClick={playing ? undefined : play}
            aria-disabled={playing}
            className="h-12 shrink-0 cursor-pointer self-start bg-ink px-8 font-semibold text-ground transition-transform duration-150 ease-out focus-visible:outline-accent active:scale-[0.97] aria-disabled:cursor-default aria-disabled:opacity-60 sm:self-auto"
          >
            Play
          </button>
        </div>
      </div>

      <p
        ref={clause}
        aria-live="polite"
        className={cn(
          "order-4 mt-2 min-h-[2.2em] self-end font-semibold tracking-tight text-balance text-[clamp(1.5rem,2.6vw,2.25rem)] leading-[1.15] lg:col-start-1 lg:row-start-3 lg:mt-12 lg:mb-10",
          sidePadding,
        )}
      >
        {stages[stage].clause}
      </p>

      <p className={cn("t-body order-5 max-w-[46ch] pt-6 pb-16 lg:col-start-1 lg:row-start-2 lg:pt-5 lg:pb-0", sidePadding)}>
        {copy.snapshot.text}
      </p>
    </div>
  );
}
