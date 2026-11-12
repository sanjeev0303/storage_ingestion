import Link from "next/link";
import { DuneHorizon } from "@/components/brand/dune-horizon";
import { Logo } from "@/components/brand/logo";
import { actions, close, footer } from "./copy";
import { Wrap } from "./wrap";

// The close and the footer share one night field, with the dune horizon between them.
export function Closing() {
  const year = new Date().getFullYear();
  return (
    <footer className="field-night relative overflow-hidden" aria-labelledby="close-title">
      {/* the orbit line's last stretch, from the section above down to the close */}
      <span aria-hidden className="orbit-line orbit-tail" />
      <Wrap className="relative z-[1] pt-24 lg:pt-32">
        <div className="max-w-3xl">
          <h2 id="close-title" className="t-h2 orbit-stop">
            {close.heading}
          </h2>
          <p className="t-lead mt-5 max-w-[38rem] text-[var(--on-field-muted)]">{close.text}</p>
          <div className="mt-9 flex flex-wrap items-center gap-x-6 gap-y-3">
            <Link href={actions.primary.href} className="btn-orb">
              {actions.primary.label}
            </Link>
            <Link href={actions.signIn.href} className="link-orb text-white">
              {actions.signIn.label}
            </Link>
          </div>
        </div>
      </Wrap>

      <DuneHorizon className="mt-12 h-[180px] sm:h-[240px] lg:mt-6 lg:h-[300px]" />

      <Wrap className="relative z-[1] pb-10">
        <div className="grid gap-10 pt-4 sm:grid-cols-[1fr_auto_auto] sm:gap-16">
          <div>
            <Logo />
          </div>
          {footer.columns.map((col) => (
            <nav key={col.title} aria-label={col.title}>
              <h3 className="text-sm font-semibold">{col.title}</h3>
              <ul className="mt-2">
                {col.links.map((l) => (
                  <li key={l.href}>
                    <a href={l.href} className="inline-flex min-h-11 items-center text-[0.9375rem] text-[var(--on-field-muted)] transition-colors duration-150 hover:text-white">
                      {l.label}
                    </a>
                  </li>
                ))}
              </ul>
            </nav>
          ))}
        </div>
        <p className="mt-10 flex flex-wrap gap-x-6 gap-y-1 text-sm text-[var(--on-field-muted)]">
          <span>© {year} Orbidune</span>
          <span>{footer.trademarks}</span>
        </p>
      </Wrap>
    </footer>
  );
}
