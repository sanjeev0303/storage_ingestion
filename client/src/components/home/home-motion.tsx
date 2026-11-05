"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, type ReactNode } from "react";

gsap.registerPlugin(useGSAP, ScrollTrigger);

// The page reads complete without script. With it: the hero rises in reading
// order, the accession stage opens like a shutter from its cut corner (the one
// loud moment), and each section's parts arrive once as it enters. Reduced
// motion keeps everything where it is.
export function HomeMotion({ children }: { children: ReactNode }) {
  const root = useRef<HTMLDivElement>(null);

  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        gsap.from("[data-hero]", { y: 24, opacity: 0, duration: 0.7, ease: "power3.out", stagger: 0.08 });
        gsap.fromTo(
          "[data-shutter]",
          { clipPath: "inset(0% 100% 0% 0%)" },
          { clipPath: "inset(0% 0% 0% 0%)", duration: 1.1, ease: "power3.inOut", delay: 0.2, clearProps: "clipPath" },
        );
        gsap.from("[data-settle]", { scale: 1.06, transformOrigin: "left top", duration: 1.1, ease: "power3.out", delay: 0.2 });

        gsap.set("[data-reveal]", { y: 24, opacity: 0 });
        ScrollTrigger.batch("[data-reveal]", {
          start: "top 92%",
          once: true,
          onEnter: (batch) => gsap.to(batch, { y: 0, opacity: 1, duration: 0.6, ease: "power3.out", stagger: 0.07 }),
        });
      });
    },
    { scope: root },
  );

  // Clip keeps the hero stage's bleed from widening the page on a phone.
  return (
    <div ref={root} className="home overflow-x-clip">
      {children}
    </div>
  );
}
