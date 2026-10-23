"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRef, useState } from "react";
import { cn } from "@/lib/utils/cn";
import { copy } from "../copy";
import { ValidateGrid } from "../diagrams/validate-grid";

gsap.registerPlugin(useGSAP, ScrollTrigger);

export function ValidateBody() {
  const [mode, setMode] = useState(0);
  const root = useRef<HTMLDivElement>(null);

  // One trigger per statement; the one crossing the reading line sets the mode.
  useGSAP(
    () => {
      gsap.utils.toArray<HTMLElement>("[data-statement]", root.current).forEach((block, index) => {
        ScrollTrigger.create({
          trigger: block,
          start: "top 55%",
          end: "bottom 55%",
          onEnter: () => setMode(index),
          onEnterBack: () => setMode(index),
        });
      });
    },
    { scope: root },
  );

  return (
    <div ref={root} className="grid lg:grid-cols-12 lg:gap-x-8">
      <h2 id="validate-title" className="t-h2 lg:col-span-5 lg:col-start-1 lg:row-start-1" data-reveal="left">
        {copy.validate.title}
      </h2>

      <div className="sticky top-0 z-10 -mx-5 mt-6 flex h-[46svh] items-center bg-ground px-5 sm:-mx-8 sm:px-8 lg:z-auto lg:mx-0 lg:mt-0 lg:block lg:h-auto lg:self-start lg:bg-transparent lg:px-0 lg:col-span-6 lg:col-start-7 lg:row-span-4 lg:row-start-1 lg:top-[14vh]">
        <ValidateGrid mode={mode} />
      </div>

      <div className="lg:col-span-5 lg:col-start-1 lg:row-start-2">
        {copy.validate.statements.map((statement, index) => (
          <div
            key={statement.mode}
            data-statement
            className="flex min-h-[60svh] flex-col justify-start pt-8 lg:min-h-[55vh] lg:pt-12"
          >
            <h3
              className={cn(
                "t-lead m-0 font-semibold transition-colors duration-200",
                index === mode ? "text-ink" : "text-muted",
              )}
            >
              {statement.mode}
            </h3>
            <p
              className={cn(
                "t-body mt-3 max-w-[42ch] transition-colors duration-200",
                index === mode ? "text-ink" : "text-muted",
              )}
            >
              {statement.text}
            </p>
          </div>
        ))}
      </div>
    </div>
  );
}
