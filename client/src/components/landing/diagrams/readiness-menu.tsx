"use client";

import {
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
} from "react";
import { Outcome, type Outcome as OutcomeName } from "@/components/ui/status";
import { cn } from "@/lib/utils/cn";
import { copy } from "../copy";

const { dimensions, statuses, coverage } = copy.readiness;

const outcomes: OutcomeName[] = ["passed", "warning", "failed", "not_assessed"];

// The console's command menu scaled up. Roving buttons: arrows move the
// selection from the filter or from the list, Escape clears the filter.
export function ReadinessMenu() {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState<string>(dimensions[0].name);
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const block = useRef<HTMLDivElement>(null);
  const buttons = useRef<Record<string, HTMLButtonElement | null>>({});
  const fromKeyRepeat = useRef(false);

  const needle = query.trim().toLowerCase();
  const shown = dimensions.filter((d) => d.name.toLowerCase().includes(needle));
  const current = shown.find((d) => d.name === active) ?? shown[0];
  const currentName = current?.name;

  // The amber block slides to the selected name with a transform.
  useLayoutEffect(() => {
    const place = () => {
      const el = block.current;
      const button = currentName ? buttons.current[currentName] : null;
      if (!el) return;
      if (!button) {
        el.style.opacity = "0";
        return;
      }
      el.style.opacity = "1";
      el.style.height = `${button.offsetHeight}px`;
      el.style.transitionDuration = fromKeyRepeat.current ? "0ms" : "";
      el.style.transform = `translateY(${button.offsetTop}px)`;
    };
    place();
    const observer = new ResizeObserver(place);
    if (list.current) observer.observe(list.current);
    return () => observer.disconnect();
  }, [currentName, needle]);

  function move(step: 1 | -1, fromList: boolean) {
    const at = shown.findIndex((d) => d.name === currentName);
    const next = shown[Math.min(Math.max(at + step, 0), shown.length - 1)];
    setActive(next.name);
    if (fromList) buttons.current[next.name]?.focus();
  }

  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    fromKeyRepeat.current = event.repeat;
    const fromList = event.target !== input.current;
    if (event.key === "Escape") {
      setQuery("");
      input.current?.focus();
    } else if (
      (event.key === "ArrowDown" || event.key === "ArrowUp") &&
      shown.length > 0
    ) {
      event.preventDefault();
      move(event.key === "ArrowDown" ? 1 : -1, fromList);
    } else if (event.key === "Enter" && !fromList && currentName) {
      event.preventDefault();
      setActive(currentName);
      buttons.current[currentName]?.focus();
    }
  }

  return (
    <div data-ui onKeyDown={onKeyDown}>
      <label htmlFor={`${id}-filter`} className="t-small block font-medium">
        Filter dimensions
      </label>
      <input
        ref={input}
        id={`${id}-filter`}
        type="text"
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        name="filter"
        autoComplete="off"
        spellCheck={false}
        placeholder="Type a name, for example Integrity"
        aria-controls={`${id}-list`}
        className="t-lead mt-2 h-14 w-full rounded-sm bg-faint px-4 text-ink placeholder:text-muted"
      />
      <div className="t-small mt-2 flex flex-wrap items-center justify-between gap-x-6 gap-y-1 text-muted">
        <p role="status" aria-live="polite">
          {shown.length} of {dimensions.length} dimensions shown.
          <span className="sr-only">
            {current ? ` ${current.name} selected.` : ""}
          </span>
        </p>
        <p className="hidden items-center gap-2 sm:flex" aria-hidden>
          <kbd className="rounded-sm bg-faint px-2 py-0.5 font-sans text-ink">Up</kbd>
          <kbd className="rounded-sm bg-faint px-2 py-0.5 font-sans text-ink">Down</kbd>
          move
          <kbd className="rounded-sm bg-faint px-2 py-0.5 font-sans text-ink">Enter</kbd>
          select
          <kbd className="rounded-sm bg-faint px-2 py-0.5 font-sans text-ink">Esc</kbd>
          clear
        </p>
      </div>

      <div className="relative mt-6">
        <div
          ref={block}
          aria-hidden
          className="absolute top-0 left-0 h-0 w-full rounded-sm bg-accent transition-transform duration-[180ms] ease-[cubic-bezier(0.23,1,0.32,1)] motion-reduce:transition-none"
        />
        <ul
          id={`${id}-list`}
          ref={list}
          aria-label="Readiness dimensions"
          className="relative"
        >
          {shown.map((dimension) => {
            const selected = dimension.name === currentName;
            return (
              <li key={dimension.name}>
                <button
                  type="button"
                  ref={(el) => {
                    buttons.current[dimension.name] = el;
                  }}
                  aria-expanded={selected}
                  aria-controls={selected ? `${id}-detail` : undefined}
                  onClick={() => {
                    fromKeyRepeat.current = false;
                    setActive(dimension.name);
                  }}
                  className={cn(
                    "relative block min-h-11 w-full rounded-sm px-4 py-1 text-left text-[clamp(1.5rem,2.8vw,2.5rem)] leading-[1.1] font-semibold tracking-tight",
                    selected ? "text-on-accent" : "hover:bg-faint",
                  )}
                >
                  {dimension.name}
                </button>
                {selected && (
                  <div id={`${id}-detail`} className="px-4 pt-3 pb-6">
                    <p className="t-body max-w-xl">{dimension.text}</p>
                    <ul
                      aria-label="Example statuses"
                      className="mt-4 flex flex-wrap gap-2"
                    >
                      {statuses.map((status, i) => (
                        <li
                          key={status}
                          className="t-small flex items-center gap-3 rounded-sm bg-faint px-3 py-2"
                        >
                          <Outcome outcome={outcomes[i]} label={status} />
                          <span className="text-muted">{coverage[i]}</span>
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
              </li>
            );
          })}
        </ul>
        {shown.length === 0 && (
          <p className="t-body px-4 py-3">No dimension matches that name.</p>
        )}
      </div>
    </div>
  );
}
