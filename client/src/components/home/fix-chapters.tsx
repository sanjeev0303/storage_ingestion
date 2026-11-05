import type { ComponentType } from "react";
import { AdoptFlow } from "@/components/landing/diagrams/adopt-flow";
import { CopiesFlow } from "@/components/landing/diagrams/copies-flow";
import { InspectFlow } from "@/components/landing/diagrams/inspect-flow";
import { LockFlow } from "@/components/landing/diagrams/lock-flow";
import { ProveFlow } from "@/components/landing/diagrams/prove-flow";
import { StreamFlow } from "@/components/landing/diagrams/stream-flow";
import { fixes } from "./home-copy";

type ChapterId = (typeof fixes.chapters)[number]["id"];

// Each problem's fix as a working diagram: pick an option, the path changes.
const FLOW: Record<ChapterId, ComponentType> = {
  inspect: InspectFlow,
  adopt: AdoptFlow,
  stream: StreamFlow,
  lock: LockFlow,
  copies: CopiesFlow,
  prove: ProveFlow,
};

export function FixChapters() {
  return (
    <div id="fixes">
      <section aria-labelledby="fixes-title" className="field-b pt-24 pb-4 lg:pt-36">
        <div className="mx-auto grid max-w-[1440px] gap-6 px-4 sm:px-10 lg:grid-cols-12">
          <h2 id="fixes-title" data-reveal className="h-head lg:col-span-6">
            {fixes.heading}
          </h2>
          <p data-reveal className="h-body max-w-[46ch] lg:col-span-5 lg:col-start-8 lg:self-end">
            {fixes.intro}
          </p>
        </div>
      </section>

      {fixes.chapters.map((c, i) => {
        const Flow = FLOW[c.id];
        const white = i % 2 === 1;
        return (
          <section
            key={c.id}
            id={`fix-${c.id}`}
            aria-labelledby={`fix-${c.id}-title`}
            className={`${white ? "field-b" : "field-a"} scroll-mt-4 py-20 lg:py-28`}
          >
            <div className="mx-auto max-w-[1440px] px-4 sm:px-10">
              <div className="grid gap-6 lg:grid-cols-12">
                <h3 id={`fix-${c.id}-title`} data-reveal className="h-head lg:col-span-6">
                  {c.problem}
                </h3>
                <p data-reveal className="h-body max-w-[52ch] lg:col-span-5 lg:col-start-8 lg:self-end">
                  {c.fix}
                </p>
              </div>
              <div data-reveal className="mt-12 lg:mt-16">
                <Flow />
                <p className="h-small mt-6">{fixes.examples}</p>
              </div>
            </div>
          </section>
        );
      })}
    </div>
  );
}
