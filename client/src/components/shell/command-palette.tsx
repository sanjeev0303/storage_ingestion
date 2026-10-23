"use client";

import { MagnifyingGlass } from "@phosphor-icons/react";
import { useRouter } from "next/navigation";
import { useEffect, useState, type KeyboardEvent } from "react";
import { Button } from "@/components/ui/button";
import { Modal } from "@/components/ui/dialog";
import { Input } from "@/components/ui/field";
import { useSession } from "@/lib/auth/session";
import { NAV_PERMISSIONS } from "@/lib/permissions";
import { cn } from "@/lib/utils/cn";
import { NAV_ITEMS } from "./nav-items";

const ACCOUNT = { key: "account", label: "Account and security", href: "/app/settings/account" };

// Jump to any area by typing a few letters, so nobody has to remember where it lives.
export function CommandPalette() {
  const router = useRouter();
  const { canAny } = useSession();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);

  useEffect(() => {
    function onKey(event: globalThis.KeyboardEvent) {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setOpen((value) => !value);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const places = [...NAV_ITEMS.filter((item) => canAny(NAV_PERMISSIONS[item.key] ?? [])), ACCOUNT];
  const matches = places.filter((place) => place.label.toLowerCase().includes(query.trim().toLowerCase()));

  function change(next: boolean) {
    setOpen(next);
    setQuery("");
    setActive(0);
  }

  function go(href: string) {
    change(false);
    router.push(href);
  }

  function onKeyDown(event: KeyboardEvent) {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setActive((active + 1) % Math.max(matches.length, 1));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setActive((active - 1 + matches.length) % Math.max(matches.length, 1));
    } else if (event.key === "Enter" && matches[active]) {
      event.preventDefault();
      go(matches[active].href);
    }
  }

  return (
    <>
      <Button onClick={() => setOpen(true)} aria-label="Go to a page" className="px-3">
        <MagnifyingGlass aria-hidden />
        <span className="hidden sm:inline">Go to</span>
        <kbd className="mono-data hidden text-sm text-muted md:inline">Ctrl K</kbd>
      </Button>
      <Modal open={open} onOpenChange={change} title="Go to" description="Type to filter, arrow keys to choose, Enter to open.">
        <Input
          role="combobox"
          aria-expanded
          aria-controls="palette-list"
          aria-activedescendant={matches[active] ? `palette-${matches[active].key}` : undefined}
          aria-label="Page name"
          placeholder="Datasets, Audit, Storage"
          autoComplete="off"
          spellCheck={false}
          autoFocus
          value={query}
          onChange={(event) => {
            setQuery(event.target.value);
            setActive(0);
          }}
          onKeyDown={onKeyDown}
        />
        <ul id="palette-list" role="listbox" aria-label="Pages" className="mt-3 flex flex-col gap-1">
          {matches.map((place, i) => (
            <li
              key={place.key}
              id={`palette-${place.key}`}
              role="option"
              aria-selected={i === active}
              onMouseMove={() => setActive(i)}
              onClick={() => go(place.href)}
              className={cn("flex min-h-11 cursor-pointer items-center rounded-md px-3 text-base", i === active && "bg-ink text-ground")}
            >
              {place.label}
            </li>
          ))}
          {matches.length === 0 && <li className="px-3 py-2 text-base text-muted">Nothing matches &ldquo;{query.trim()}&rdquo;.</li>}
        </ul>
      </Modal>
    </>
  );
}
