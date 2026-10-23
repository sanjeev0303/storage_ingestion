"use client";

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Outcome } from "@/components/ui/status";
import { copy } from "../copy";

const PATH = "images/cat-0412.jpg";
const [live, scheduled, held] = copy.retain.states;

function RowShell({ status, children }: { status: ReactNode; children?: ReactNode }) {
  return (
    <div data-ui className="flex w-full max-w-[640px] flex-col gap-4 rounded-sm bg-faint p-5 text-ink">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <span className="font-medium">{PATH}</span>
        {status}
      </div>
      {children}
    </div>
  );
}

function Described({ name, note, children }: { name: string; note: string; children: ReactNode }) {
  return (
    <div className="grid items-start gap-5 md:grid-cols-[minmax(0,1fr)_minmax(0,640px)] md:gap-10">
      <div>
        <h3 className="t-lead">{name}</h3>
        <p className="t-body mt-2 max-w-[34ch]">{note}</p>
      </div>
      {children}
    </div>
  );
}

function LockMark() {
  return (
    <svg viewBox="0 0 16 16" className="size-4 shrink-0" aria-hidden>
      <rect x="4.5" y="1.5" width="7" height="6" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <rect x="1.5" y="7" width="13" height="7.5" fill="currentColor" />
      <rect x="6.5" y="9.5" width="3" height="3" fill="var(--accent)" />
    </svg>
  );
}

function ScheduledRow() {
  const [step, setStep] = useState<"scheduled" | "restoring" | "live">("scheduled");
  const timer = useRef<number>(0);
  const action = useRef<HTMLButtonElement>(null);
  const touched = useRef(false);

  useEffect(() => () => window.clearTimeout(timer.current), []);

  // The button that was pressed is replaced, so hand focus to its successor.
  useEffect(() => {
    if (touched.current && step !== "restoring") action.current?.focus();
  }, [step]);

  function deleteAgain() {
    touched.current = true;
    setStep("scheduled");
  }

  function restore() {
    touched.current = true;
    setStep("restoring");
    timer.current = window.setTimeout(() => setStep("live"), 450);
  }

  const isLive = step === "live";
  const state = isLive ? live : scheduled;

  return (
    <div aria-live="polite">
      <Described name={state.name} note={state.note}>
        <RowShell
          status={
            isLive ? <Outcome outcome="passed" label="Live" /> : <Outcome outcome="warning" label="Scheduled for removal" />
          }
        >
          {isLive ? (
            <div>
              <Button ref={action} className="h-11 px-6" onClick={deleteAgain}>
                Delete again
              </Button>
            </div>
          ) : (
            <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
              <div className="flex min-w-[12rem] flex-1 items-center gap-4">
                <div className="h-2 flex-1 bg-ground" aria-hidden>
                  <div
                    className="h-full origin-left bg-accent transition-transform duration-[450ms] ease-out motion-reduce:transition-none"
                    style={{ transform: `scaleX(${step === "restoring" ? 1 : 6 / 7})` }}
                  />
                </div>
                <span className="t-small whitespace-nowrap">6 days left (example)</span>
              </div>
              <Button ref={action} className="h-11 px-6" onClick={restore} disabled={step === "restoring"}>
                Restore
              </Button>
            </div>
          )}
        </RowShell>
      </Described>
    </div>
  );
}

export function RetainStates() {
  return (
    <div className="flex flex-col gap-12">
      <Described name={live.name} note={live.note}>
        <RowShell status={<Outcome outcome="passed" label="Live" />} />
      </Described>
      <ScheduledRow />
      <Described name={held.name} note={held.note}>
        <RowShell
          status={
            <span className="inline-flex items-center gap-1.5 font-medium">
              <LockMark />
              Kept by a hold or retention rule
            </span>
          }
        />
      </Described>
    </div>
  );
}
