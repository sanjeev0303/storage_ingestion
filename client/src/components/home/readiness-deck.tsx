"use client";

import { useState } from "react";
import { OutcomeMark } from "@/components/ui/status";
import { readiness } from "./home-copy";

const TONES = ["bg-[var(--h-tint-1)]", "bg-[var(--h-tint-2)]", "bg-[var(--h-tint-3)]"];

const MARK = { Pass: "passed", Warn: "warning", Fail: "failed", "Not checked": "not_assessed" } as const;

function Status({ status }: { status: keyof typeof MARK }) {
  return (
    <span className="inline-flex items-center gap-2 font-medium">
      <OutcomeMark outcome={MARK[status]} />
      {status}
    </span>
  );
}

// Cards that fold into spines: the open dimension shows in full, the others
// stand beside it as labels with their name and status set up the spine.
export function ReadinessDeck() {
  const [open, setOpen] = useState(1);
  return (
    <section id="readiness" aria-labelledby="readiness-title" className="field-b py-24 lg:py-36">
      <div className="mx-auto max-w-[1440px] px-4 sm:px-10">
        <div className="grid gap-6 lg:grid-cols-12">
          <h2 id="readiness-title" data-reveal className="h-head lg:col-span-6">
            {readiness.heading}
          </h2>
          <p data-reveal className="h-body max-w-[46ch] lg:col-span-5 lg:col-start-8 lg:self-end">
            {readiness.intro}
          </p>
        </div>

        <ul data-reveal className="mt-14 hidden h-[400px] gap-[6px] lg:flex">
          {readiness.dimensions.map((d, i) => {
            const isOpen = i === open;
            return (
              <li
                key={d.name}
                className={`label-cut relative shrink-0 overflow-hidden text-[var(--h-ink)] transition-[flex-basis] duration-300 ease-[var(--h-ease)] [--cut:16px] motion-reduce:transition-none ${TONES[i % 3]} ${
                  isOpen ? "basis-[560px]" : "basis-[76px]"
                }`}
              >
                {isOpen ? (
                  <article className="flex h-full w-[560px] flex-col justify-between p-8">
                    <div>
                      <h3 className="h-lead">{d.name}</h3>
                      <p className="h-body mt-3 max-w-[40ch]">{d.text}</p>
                    </div>
                    <p className="h-body flex justify-between">
                      <Status status={d.status} />
                      <span className="font-medium">Coverage: {d.coverage}</span>
                    </p>
                  </article>
                ) : (
                  <button
                    type="button"
                    onClick={() => setOpen(i)}
                    aria-label={`Show ${d.name}, ${d.status}`}
                    className="flex h-full w-full cursor-pointer flex-col items-center justify-between py-6 transition-opacity duration-150 [@media(hover:hover)_and_(pointer:fine)]:hover:opacity-75"
                  >
                    <span aria-hidden className="h-body rotate-180 font-medium whitespace-nowrap [writing-mode:vertical-rl]">
                      {d.name}
                    </span>
                    <span aria-hidden className="h-small flex flex-col items-center gap-2">
                      <OutcomeMark outcome={MARK[d.status]} />
                      <span className="rotate-180 font-medium whitespace-nowrap [writing-mode:vertical-rl]">{d.status}</span>
                    </span>
                  </button>
                )}
              </li>
            );
          })}
        </ul>

        <ul className="mt-12 grid gap-3 lg:hidden">
          {readiness.dimensions.map((d, i) => (
            <li key={d.name} className={`label-cut p-6 text-[var(--h-ink)] [--cut:16px] ${TONES[i % 3]}`}>
              <h3 className="h-lead">{d.name}</h3>
              <p className="h-body mt-2">{d.text}</p>
              <p className="h-small mt-4 flex justify-between">
                <Status status={d.status} />
                <span className="font-medium">Coverage: {d.coverage}</span>
              </p>
            </li>
          ))}
        </ul>
        <p className="h-body mt-12">
          {readiness.dimensions.length} dimensions. {readiness.foot}
        </p>
      </div>
    </section>
  );
}
