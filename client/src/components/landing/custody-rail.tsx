"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef } from "react";
import type { STOPS } from "./copy";

gsap.registerPlugin(useGSAP, ScrollTrigger);

type Stops = typeof STOPS;

// The chain of custody: one square per hand-off. A stage the reader has passed
// is filled (checked), the current one is amber, the ones ahead are dashed (not
// checked yet), and an amber token rides the line between them. It is the
// outcome vocabulary of the product, used as the page's progress.
export function CustodyRail({ stops }: { stops: Stops }) {
  const root = useRef<HTMLElement>(null);

  useGSAP(
    () => {
      const nav = root.current;
      if (!nav) return;
      const items = gsap.utils.toArray<HTMLElement>("[data-rail-item]", nav);
      const fill = nav.querySelector<HTMLElement>("[data-rail-fill]");
      const token = nav.querySelector<HTMLElement>("[data-rail-token]");
      const track = nav.querySelector<HTMLElement>("[data-rail-track]");
      const bar = document.querySelector<HTMLElement>("[data-rail-bar]");
      const setState = (index: number, state: "upcoming" | "current" | "passed") => {
        items[index].dataset.state = state;
      };
      // The rail has no plate of its own, so its colour follows the band behind it.
      const setTone = (section: HTMLElement) => {
        nav.dataset.tone = section.dataset.tone ?? "ground";
      };

      stops.forEach((stop, index) => {
        const section = document.getElementById(stop.id);
        if (!section) return;
        ScrollTrigger.create({
          trigger: section,
          start: "top 55%",
          end: "bottom 55%",
          onEnter: () => {
            setState(index, "current");
            setTone(section);
          },
          onLeave: () => setState(index, "passed"),
          onEnterBack: () => {
            setState(index, "current");
            setTone(section);
          },
          onLeaveBack: () => setState(index, "upcoming"),
        });
      });

      const moveToken = token ? gsap.quickSetter(token, "y", "px") : null;
      ScrollTrigger.create({
        trigger: document.querySelector("main"),
        start: "top 55%",
        end: "bottom 55%",
        onUpdate: (self) => {
          if (fill) fill.style.transform = `scaleY(${self.progress})`;
          if (bar) {
            bar.style.transform = `scaleX(${self.progress})`;
            bar.setAttribute("aria-valuenow", String(Math.round(self.progress * 100)));
          }
          if (moveToken && track) moveToken(self.progress * (track.clientHeight - 12));
        },
      });
    },
    { scope: root },
  );

  return (
    <>
      <div
        role="progressbar"
        aria-label="Position in the chain of custody"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={0}
        data-rail-bar
        className="fixed top-0 left-0 z-40 h-1 w-full origin-left scale-x-0 bg-accent lg:hidden"
      />
      <nav
        ref={root}
        data-tone="ground"
        aria-label="Stages in a dataset's chain of custody"
        className="fixed top-1/2 left-4 z-30 hidden -translate-y-1/2 px-1.5 py-3 text-ink data-[tone=amber]:text-on-accent data-[tone=plum]:text-ground lg:block"
      >
        <div data-rail-track className="absolute top-6 bottom-6 left-1/2 w-1 -translate-x-1/2 bg-current/20" aria-hidden>
          <div data-rail-fill className="h-full origin-top scale-y-0 bg-current" />
          <div data-rail-token className="absolute top-0 -left-1 size-3 border-2 border-current bg-accent" />
        </div>
        <ol className="relative flex flex-col">
          {stops.map((stop) => (
            <li key={stop.id}>
              <a
                href={`#${stop.id}`}
                data-rail-item
                data-state="upcoming"
                aria-label={stop.name}
                className="group relative grid size-7 place-items-center pointer-coarse:size-11"
              >
                <span
                  aria-hidden
                  className="block size-3 border-2 border-current transition-colors duration-150 group-data-[state=current]:size-3.5 group-data-[state=current]:bg-accent group-data-[state=passed]:bg-current group-data-[state=upcoming]:border-dashed"
                />
                <span
                  aria-hidden
                  className="t-small pointer-events-none absolute left-9 bg-accent px-2 py-1 font-medium whitespace-nowrap text-on-accent opacity-0 transition-opacity duration-150 group-focus-visible:opacity-100 group-data-[state=current]:opacity-100 [@media(hover:hover)]:group-hover:opacity-100"
                >
                  {stop.name}
                </span>
              </a>
            </li>
          ))}
        </ol>
      </nav>
    </>
  );
}
