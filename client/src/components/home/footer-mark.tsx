import Link from "next/link";
import { footer } from "./home-copy";
import { Barcode } from "./motif";

// The page closes on the motif at its largest: the name as an accession
// label, corner cut, with the barcode of the line it stands for.
export function FooterMark() {
  return (
    <footer className="field-b pt-20 pb-10">
      <div className="mx-auto max-w-[1440px] px-4 sm:px-10">
        <div className="label-cut flex flex-col justify-between gap-10 bg-[var(--h-orange)] p-6 text-[var(--h-night)] [--cut:56px] sm:p-10 lg:min-h-[34svh] lg:[--cut:120px] lg:pl-[150px]">
          <p className="h-display">Object Storage</p>
          <div className="flex flex-wrap items-end justify-between gap-6">
            <p className="h-body max-w-[34ch]">{footer.line}</p>
            <Barcode value={footer.line} className="h-12 w-full max-w-[22rem]" />
          </div>
        </div>

        <div className="mt-14 grid gap-10 sm:grid-cols-2 lg:grid-cols-12">
          {footer.columns.map((col) => (
            <nav key={col.title} aria-label={col.title} className="lg:col-span-2">
              <h2 className="h-small font-medium">{col.title}</h2>
              <ul className="mt-3 grid gap-2">
                {col.links.map((l) => (
                  <li key={l.href}>
                    <Link href={l.href} className="h-small h-link">
                      {l.label}
                    </Link>
                  </li>
                ))}
              </ul>
            </nav>
          ))}
        </div>
        <div className="mt-14 flex items-center justify-between gap-4">
          <p className="h-small">Object Storage</p>
          <Link href="/login" className="h-button h-small">
            Sign in
          </Link>
        </div>
      </div>
    </footer>
  );
}
