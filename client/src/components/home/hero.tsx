import Link from "next/link";
import { AccessionSlip } from "./accession-slip";
import { hero } from "./home-copy";

// First viewport: the statement left, and the accession stage, a large label
// with its corner cut, bleeding off the right and bottom edges.
export function Hero() {
  const [first, second] = hero.title.split(" checks ");
  return (
    <section id="accession" aria-labelledby="hero-title" className="field-b relative overflow-hidden">
      <div className="mx-auto max-w-[1440px] px-4 sm:px-10 lg:grid lg:grid-cols-12 lg:gap-x-6">
        <div className="pt-14 lg:col-span-5 lg:col-start-1 lg:row-start-1 lg:pt-24 lg:pb-16">
          <h1 id="hero-title" data-hero className="h-display">
            <span className="block">{first}</span>
            <span className="block">checks {second}</span>
          </h1>
          <p data-hero className="h-body mt-8 max-w-[40ch]">
            {hero.sentence}
          </p>
          <div data-hero className="mt-8">
            <Link href="/login" className="h-button">
              {hero.action}
              <svg viewBox="0 0 16 16" className="size-4" aria-hidden>
                <path d="M3 13L13 3M6 3h7v7" fill="none" stroke="currentColor" strokeWidth="2" />
              </svg>
            </Link>
          </div>
        </div>

        <div data-shutter className="relative mt-14 lg:col-span-8 lg:col-start-5 lg:row-start-1 lg:mt-[34vh]">
          <div className="field-mark label-cut absolute inset-y-0 -left-4 -right-[100vw] [--cut:72px] sm:-left-10 lg:left-0 lg:[--cut:128px]" />
          <div data-settle className="relative px-0 pt-20 pb-16 text-[var(--h-mark-ink)] lg:px-14 lg:pt-[92px]">
            <AccessionSlip />
          </div>
        </div>
      </div>
    </section>
  );
}
