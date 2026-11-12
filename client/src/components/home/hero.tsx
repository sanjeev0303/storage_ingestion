import Link from "next/link";
import { actions, hero } from "./copy";
import { FigureOne } from "./figure-one";
import { Wrap } from "./wrap";

export function Hero() {
  return (
    // pt-20 keeps the content clear of the fixed header that sits over the hero.
    <section id="hero" className="field-ultra relative flex min-h-svh flex-col overflow-hidden pt-20" aria-labelledby="hero-title">
      <Wrap className="grid flex-1 items-center gap-x-10 gap-y-12 pt-8 pb-16 lg:grid-cols-12 lg:pt-4 lg:pb-20">
        <div className="lg:col-span-6" data-enter>
          <h1 id="hero-title" className="t-display">
            {hero.title}
          </h1>
          <p className="t-lead mt-6 max-w-[34rem] text-[var(--on-field-muted)]">{hero.lead}</p>
          <div className="mt-9 flex flex-wrap items-center gap-x-6 gap-y-3">
            <Link href={actions.primary.href} className="btn-orb">
              {actions.primary.label}
            </Link>
            <a href={hero.secondary.href} className="link-orb text-white">
              {hero.secondary.label}
            </a>
          </div>
          <p className="t-small mt-10 max-w-[30rem] border-t border-[var(--on-field-line)] pt-4 text-[var(--on-field-muted)]">
            {hero.fact}
          </p>
        </div>
        <div className="lg:col-span-6 xl:pl-4" data-enter>
          <FigureOne />
        </div>
      </Wrap>
    </section>
  );
}
