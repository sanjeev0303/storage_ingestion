import { stages } from "./home-copy";

// The stages as one bar across the page. There are no measured durations to
// size them by, so they share the width; each is its own flat tone.
const TONES = [
  "bg-[var(--h-b)] text-[var(--h-ink)] outline-2 -outline-offset-2 outline-[var(--h-ink)]",
  "bg-[var(--h-tint-2)] text-[var(--h-ink)]",
  "bg-[var(--h-orange)] text-[var(--h-night)]",
  "bg-[#9a3d0a] text-[#fff4ea]",
  "bg-[var(--h-night)] text-[var(--h-orange)]",
];

export function StageBar() {
  return (
    <section id="stages" aria-labelledby="stages-title" className="field-b py-24 lg:py-36">
      <div className="mx-auto max-w-[1440px] px-4 sm:px-10">
        <h2 id="stages-title" data-reveal className="h-head max-w-[16ch]">
          {stages.heading}
        </h2>
        <ol data-ui className="mt-16 grid gap-[6px] lg:grid-cols-5">
          {stages.steps.map((step, i) => (
            <li key={step.name} data-reveal className="flex flex-col">
              <div className={`flex min-h-[72px] flex-col justify-between p-5 lg:min-h-[22vh] ${TONES[i]}`}>
                <span className="h-lead">{step.name}</span>
                <span className="h-small mt-6 font-medium">{step.inside}</span>
              </div>
              <p className="h-body mt-4 max-w-[22ch]">{step.clause}</p>
            </li>
          ))}
        </ol>
      </div>
    </section>
  );
}
