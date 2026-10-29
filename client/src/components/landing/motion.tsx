"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, type ReactNode } from "react";

gsap.registerPlugin(useGSAP, ScrollTrigger);

// The page reads fine without script. With it, the hero arrives in reading
// order and marked blocks reveal as they scroll into view. Diagrams and the
// custody rail run their own timelines. Reduced motion keeps everything still.
export function LandingMotion({ children }: { children: ReactNode }) {
  const root = useRef<HTMLDivElement>(null);

  useGSAP(
    () => {
      const mm = gsap.matchMedia();

      mm.add("(prefers-reduced-motion: no-preference)", () => {
        gsap.from("[data-hero]", { y: 24, autoAlpha: 0, duration: 0.7, ease: "power3.out", stagger: 0.08 });
        // Each block arrives from the side its text sits on; the last ones only fade.
        const from = {
          left: { x: -32 },
          right: { x: 32 },
          fade: {},
        } as const;
        gsap.utils.toArray<HTMLElement>("[data-reveal]").forEach((el) => {
          const kind = (el.dataset.reveal as keyof typeof from) in from ? (el.dataset.reveal as keyof typeof from) : "fade";
          // opacity, not autoAlpha: a block hidden with visibility would drop
          // its links and fields out of the Tab order until scrolled to.
          gsap.from(el, {
            ...from[kind],
            opacity: 0,
            duration: 0.8,
            ease: "power3.out",
            scrollTrigger: { trigger: el, start: "top 85%", once: true },
          });
        });
      });
    },
    { scope: root },
  );

  // Blocks start up to 32px to the side; clip keeps that from widening the
  // page on a phone, and unlike hidden it leaves the sticky diagrams sticky.
  return (
    <div ref={root} className="overflow-x-clip">
      {children}
    </div>
  );
}
