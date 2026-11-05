import Link from "next/link";
import { start } from "./home-copy";

// One word is the button: the whole band is the link. Hover and focus swap
// the field and the word.
export function StartBand() {
  return (
    <Link
      href={start.href}
      className="label-cut relative block min-h-[46svh] overflow-hidden bg-[var(--h-night)] text-[var(--h-orange)] transition-colors duration-150 [--cut:72px] hover:bg-[var(--h-orange)] hover:text-[var(--h-night)] focus-visible:bg-[var(--h-orange)] focus-visible:text-[var(--h-night)] focus-visible:outline-offset-[-6px] lg:min-h-[60svh] lg:[--cut:160px]"
    >
      <span className="h-body absolute top-20 right-16 left-4 max-w-[30ch] sm:left-10 lg:top-10 lg:left-[200px]">{start.sentence}</span>
      <svg viewBox="0 0 48 48" className="absolute top-6 right-4 size-8 sm:right-10 lg:top-10 lg:size-12" aria-hidden>
        <path d="M10 38L38 10M16 10h22v22" fill="none" stroke="currentColor" strokeWidth="4" />
      </svg>
      <span className="absolute right-4 bottom-6 left-4 block text-[clamp(6rem,36vw,34rem)] leading-[0.8] font-medium tracking-[-0.05em] sm:left-10">
        {start.word}
      </span>
    </Link>
  );
}
