import Link from "next/link";
import { copy } from "../copy";
import { Section } from "../section";
import { HeroReport } from "./hero-report";

export function Hero() {
  return (
    <Section stop="question" tone="ground" labelledBy="question-title" className="pt-10 lg:pt-16">
      <div className="grid items-start gap-12 lg:grid-cols-12 lg:gap-x-8">
        <div className="lg:col-span-6">
          <h1 id="question-title" data-hero className="t-display">
            {copy.hero.title}
          </h1>
          <p data-hero className="t-body mt-8 max-w-[44ch] text-pretty">
            {copy.hero.sentence}
          </p>
          <div data-hero className="mt-10 flex flex-wrap items-center gap-x-8 gap-y-3">
            <Link
              href="/login"
              className="inline-flex h-12 items-center bg-accent px-8 text-md font-semibold text-on-accent transition-[transform,opacity] duration-150 active:scale-[0.97] [@media(hover:hover)]:hover:opacity-90"
            >
              {copy.hero.primary}
            </Link>
            <a href="#connect" className="inline-flex min-h-12 items-center text-md font-semibold underline underline-offset-4">
              {copy.hero.secondary}
            </a>
          </div>
        </div>
        <div className="lg:col-span-6">
          <HeroReport />
        </div>
      </div>
    </Section>
  );
}
