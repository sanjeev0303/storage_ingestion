"use client";

import { useRef, useState, type KeyboardEvent } from "react";
import { cn } from "@/lib/utils/cn";
import { copy } from "../copy";
import { BANDS, ROLE_PERMISSIONS } from "./access-data";

const roles = copy.access.roles;
const total = BANDS.reduce((sum, band) => sum + band.permissions.length, 0);

export function AccessRoles() {
  const [active, setActive] = useState(0);
  const [open, setOpen] = useState<Set<string>>(() => new Set([BANDS[0].name]));
  const tabs = useRef<(HTMLButtonElement | null)[]>([]);

  function toggle(name: string) {
    setOpen((previous) => {
      const next = new Set(previous);
      if (!next.delete(name)) next.add(name);
      return next;
    });
  }

  function onKeyDown(event: KeyboardEvent) {
    const last = roles.length - 1;
    let next = active;
    if (event.key === "ArrowDown" || event.key === "ArrowRight") next = active === last ? 0 : active + 1;
    else if (event.key === "ArrowUp" || event.key === "ArrowLeft") next = active === 0 ? last : active - 1;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = last;
    else return;
    event.preventDefault();
    setActive(next);
    tabs.current[next]?.focus();
  }

  const role = roles[active];
  const allowed = new Set<string>(ROLE_PERMISSIONS[role.name]);

  return (
    <div className="flex flex-col gap-6 lg:flex-row lg:gap-8">
      <div
        role="tablist"
        aria-label="Built-in roles"
        onKeyDown={onKeyDown}
        className="grid grid-cols-2 gap-1.5 sm:grid-cols-4 lg:flex lg:w-[150px] lg:shrink-0 lg:flex-col"
      >
        {roles.map((item, i) => (
          <button
            key={item.name}
            ref={(el) => {
              tabs.current[i] = el;
            }}
            role="tab"
            id={`role-tab-${i}`}
            aria-selected={i === active}
            aria-controls="role-panel"
            tabIndex={i === active ? 0 : -1}
            onClick={() => setActive(i)}
            className={cn(
              "flex h-16 w-[150px] shrink-0 flex-col items-start justify-center px-4 text-left font-semibold transition-colors duration-150 active:scale-[0.97] focus-visible:outline-accent",
              i === active ? "bg-accent text-on-accent" : "bg-ground/10 text-ground hover:bg-ground/20",
            )}
          >
            <span className="t-body leading-tight">{item.name}</span>
            <span className="t-small font-normal">
              {ROLE_PERMISSIONS[item.name].length} of {total}
            </span>
          </button>
        ))}
      </div>

      <div role="tabpanel" id="role-panel" aria-labelledby={`role-tab-${active}`} tabIndex={0} className="min-w-0 flex-1">
        <p className="t-lead max-w-[36ch]">{role.text}</p>
        <div className="mt-6 flex flex-col gap-1.5">
          {BANDS.map((band) => {
            const count = band.permissions.filter((permission) => allowed.has(permission.id)).length;
            const isOpen = open.has(band.name);
            const listId = `band-${band.name.toLowerCase()}`;
            return (
              <div key={band.name} className="bg-ground/10">
                <h3 className="m-0">
                  <button
                    type="button"
                    aria-expanded={isOpen}
                    aria-controls={listId}
                    onClick={() => toggle(band.name)}
                    className="flex min-h-14 w-full cursor-pointer items-center justify-between gap-4 px-5 py-3 text-left focus-visible:outline-accent"
                  >
                    <span className="t-body font-semibold">{band.name}</span>
                    <span className="t-body">
                      {count} of {band.permissions.length} allowed
                    </span>
                  </button>
                </h3>
                {isOpen && (
                  <ul id={listId} className="grid gap-x-8 px-5 pb-4 sm:grid-cols-2">
                    {band.permissions.map((permission) => {
                      const can = allowed.has(permission.id);
                      return (
                        <li key={permission.id} className={cn("py-1.5", !can && "opacity-75")}>
                          <span className="t-body block leading-snug">
                            <span className="font-semibold">{can ? "Can" : "Cannot"}</span> {permission.label.toLowerCase()}
                          </span>
                          <span className="mono-data t-small block opacity-90">{permission.id}</span>
                        </li>
                      );
                    })}
                  </ul>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
