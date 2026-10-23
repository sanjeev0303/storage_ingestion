import type { ReactNode } from "react";
import { cn } from "@/lib/utils/cn";

// Check outcomes are told apart by shape and by word, never by hue alone.
// The palette has three colours, so the shapes carry the meaning:
//   passed        filled square
//   partial       half-filled square (sampled or incomplete evidence)
//   warning       square ring with an amber centre
//   failed        amber square with a cross
//   not assessed  dashed empty square
export type Outcome = "passed" | "partial" | "warning" | "failed" | "not_assessed";

const labels: Record<Outcome, string> = {
  passed: "Passed",
  partial: "Partial",
  warning: "Warning",
  failed: "Failed",
  not_assessed: "Not checked",
};

export function OutcomeMark({ outcome, className }: { outcome: Outcome; className?: string }) {
  const common = "size-4 shrink-0";
  return (
    <svg viewBox="0 0 16 16" className={cn(common, className)} aria-hidden>
      {outcome === "passed" && <rect x="1.5" y="1.5" width="13" height="13" fill="currentColor" />}
      {outcome === "partial" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <rect x="1.5" y="1.5" width="6.5" height="13" fill="currentColor" />
        </>
      )}
      {outcome === "warning" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <rect x="5" y="5" width="6" height="6" fill="var(--accent)" />
        </>
      )}
      {outcome === "failed" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="var(--accent)" stroke="currentColor" strokeWidth="1.5" />
          <path d="M4.5 4.5l7 7M11.5 4.5l-7 7" stroke="currentColor" strokeWidth="1.8" />
        </>
      )}
      {outcome === "not_assessed" && (
        <rect
          x="1.5"
          y="1.5"
          width="13"
          height="13"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeDasharray="2.5 2"
        />
      )}
    </svg>
  );
}

export function Outcome({ outcome, label }: { outcome: Outcome; label?: ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap font-medium">
      <OutcomeMark outcome={outcome} />
      {label ?? labels[outcome]}
    </span>
  );
}

// A generic state word (job state, connection status, audit outcome). Like the
// check outcomes, it carries a shape as well as a word, so states that share a
// fill still differ:
//   failed     amber pill with a crossed square
//   warning    quiet pill with a ringed square (needs a look, not broken)
//   active     recessed pill with a play triangle (in progress)
//   done       plum pill with a filled square
//   neutral    quiet pill, no mark
// "attention" is the older name for failed or warning: the word decides which.
type Tone = "neutral" | "attention" | "active" | "done" | "failed" | "warning";

const tones: Record<Exclude<Tone, "attention">, string> = {
  neutral: "bg-faint text-ink",
  warning: "bg-faint text-ink",
  active: "neo-in-sm text-ink",
  done: "bg-ink text-ground",
  failed: "bg-accent text-on-accent",
};

const FAILURE_WORDS = /fail|reject|denied|error|invalid|broken|exhaust|delet|refus|unauthori|abort|cancel/i;

function resolveTone(tone: Tone, text: string): Exclude<Tone, "attention"> {
  if (tone !== "attention") return tone;
  return FAILURE_WORDS.test(text) ? "failed" : "warning";
}

function textOf(node: ReactNode): string {
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(textOf).join(" ");
  return "";
}

function StateMark({ tone }: { tone: Exclude<Tone, "attention" | "neutral"> }) {
  return (
    <svg viewBox="0 0 16 16" className="size-3 shrink-0" aria-hidden>
      {tone === "done" && <rect x="2" y="2" width="12" height="12" fill="currentColor" />}
      {tone === "warning" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="2" />
          <rect x="5.5" y="5.5" width="5" height="5" fill="var(--accent)" />
        </>
      )}
      {tone === "failed" && (
        <>
          <rect x="1.5" y="1.5" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="2" />
          <path d="M4.5 4.5l7 7M11.5 4.5l-7 7" stroke="currentColor" strokeWidth="2" />
        </>
      )}
      {tone === "active" && <path d="M3.5 2l10 6-10 6z" fill="currentColor" />}
    </svg>
  );
}

export function StateBadge({ children, tone = "neutral" }: { children: ReactNode; tone?: Tone }) {
  const resolved = resolveTone(tone, textOf(children));
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-sm px-2 py-0.5 text-sm font-semibold whitespace-nowrap",
        tones[resolved],
      )}
    >
      {resolved !== "neutral" && <StateMark tone={resolved} />}
      {children}
    </span>
  );
}
