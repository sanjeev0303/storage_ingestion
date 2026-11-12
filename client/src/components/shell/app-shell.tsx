"use client";

import { CaretDown, List, SignOut, UserCircle } from "@phosphor-icons/react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState, type ReactNode } from "react";
import { Logo } from "@/components/brand/logo";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { Modal } from "@/components/ui/dialog";
import {
  DropdownContent,
  DropdownItem,
  DropdownLabel,
  DropdownMenu,
  DropdownSeparator,
  DropdownTrigger,
} from "@/components/ui/menu";
import { Skeleton } from "@/components/ui/states";
import { useSession } from "@/lib/auth/session";
import { NAV_PERMISSIONS } from "@/lib/permissions";
import { cn } from "@/lib/utils/cn";
import { CommandPalette } from "./command-palette";
import { NAV_GROUPS, NAV_ITEMS } from "./nav-items";
import { OrganizationGate } from "./organization-gate";

function Nav({ onNavigate }: { onNavigate?: () => void }) {
  const pathname = usePathname();
  const { canAny } = useSession();
  const items = NAV_ITEMS.filter((item) => canAny(NAV_PERMISSIONS[item.key] ?? []));

  return (
    <nav aria-label="Main" className="flex flex-col gap-5">
      {NAV_GROUPS.map((group) => {
        const inGroup = items.filter((item) => item.group === group);
        if (inGroup.length === 0) return null;
        return (
          <div key={group || "top"}>
            {group && <p className="mb-1.5 px-3 text-sm font-semibold text-muted">{group}</p>}
            <ul className="flex flex-col gap-1">
              {inGroup.map((item) => {
                const current = pathname === item.href || pathname.startsWith(`${item.href}/`);
                const Icon = item.icon;
                return (
                  <li key={item.key}>
                    <Link
                      href={item.href}
                      onClick={onNavigate}
                      aria-current={current ? "page" : undefined}
                      className={cn(
                        "relative flex min-h-11 items-center gap-3 rounded-lg border px-3 text-base font-medium transition-colors duration-150",
                        current
                          ? "border-line bg-ground text-ink before:absolute before:top-1/2 before:-left-3 before:h-5 before:w-1 before:-translate-y-1/2 before:rounded-r-full before:bg-ultra"
                          : "border-transparent text-muted hover:bg-ground/60 hover:text-ink",
                      )}
                    >
                      <Icon aria-hidden weight={current ? "fill" : "regular"} className={cn("size-5 shrink-0", current && "text-ultra-text")} />
                      {item.label}
                    </Link>
                  </li>
                );
              })}
            </ul>
          </div>
        );
      })}
    </nav>
  );
}

function OrganizationSwitcher() {
  const { memberships, membership, switchOrganization } = useSession();
  const name = membership?.organization_name ?? "Choose organization";

  if (memberships.length < 2) {
    return <span className="truncate px-1 font-semibold">{name}</span>;
  }

  return (
    <DropdownMenu>
      <DropdownTrigger asChild>
        <Button variant="secondary" className="max-w-64 min-w-0">
          <span className="truncate">{name}</span>
          <CaretDown aria-hidden className="shrink-0" />
        </Button>
      </DropdownTrigger>
      <DropdownContent align="start">
        <DropdownLabel>Switch organization</DropdownLabel>
        {memberships.map((m) => (
          <DropdownItem key={m.organization_id} onSelect={() => switchOrganization(m.organization_id)}>
            <span className="truncate">{m.organization_name ?? m.organization_id}</span>
            {m.organization_id === membership?.organization_id && (
              <span className="ml-auto text-xs font-semibold">Current</span>
            )}
          </DropdownItem>
        ))}
      </DropdownContent>
    </DropdownMenu>
  );
}

function UserMenu() {
  const { user, membership, signOut } = useSession();
  return (
    <DropdownMenu>
      <DropdownTrigger asChild>
        <Button variant="secondary" aria-label="Account menu" className="max-w-56 min-w-0">
          <UserCircle aria-hidden className="shrink-0" />
          <span className="hidden truncate sm:inline">{user.display_name || user.email}</span>
        </Button>
      </DropdownTrigger>
      <DropdownContent>
        <DropdownLabel>
          <span className="block truncate font-medium text-ink">{user.email}</span>
          {membership && <span className="block">Role: {membership.role_id}</span>}
          {user.platform_role && <span className="block">Platform role: {user.platform_role}</span>}
        </DropdownLabel>
        <DropdownSeparator />
        <DropdownItem asChild>
          <Link href="/app/settings/account">Account and security</Link>
        </DropdownItem>
        <DropdownItem onSelect={() => void signOut()}>
          <SignOut aria-hidden />
          Sign out
        </DropdownItem>
      </DropdownContent>
    </DropdownMenu>
  );
}

export function AppShell({ children }: { children: ReactNode }) {
  const [menuOpen, setMenuOpen] = useState(false);

  return (
    // One tinted frame holds the sidebar and the top bar; the page is a white sheet
    // inset from the window's right and bottom edges. On a phone the sheet is the screen.
    <div className="min-h-dvh bg-frame lg:grid lg:grid-cols-[15rem_1fr]">
      <aside className="hidden lg:block">
        <div className="sticky top-0 flex h-dvh flex-col gap-6 overflow-y-auto px-3 pt-1.5 pb-5" data-lenis-prevent>
          <Link
            href="/app/overview"
            aria-label="Orbidune overview"
            className="flex h-12 items-center px-3"
            style={{ "--logo-gap": "var(--frame)" } as React.CSSProperties}
          >
            <Logo />
          </Link>
          <Nav />
        </div>
      </aside>

      <div className="flex min-h-dvh min-w-0 flex-col">
        <header className="sticky top-0 z-30 flex h-14 items-center gap-3 bg-frame px-4 sm:px-6 lg:h-12 lg:pr-2 lg:pl-0">
          <Button className="size-9 px-0 lg:hidden" aria-label="Open navigation" onClick={() => setMenuOpen(true)}>
            <List aria-hidden />
          </Button>
          <OrganizationSwitcher />
          <div className="ml-auto flex items-center gap-2">
            <CommandPalette />
            <ThemeToggle />
            <UserMenu />
          </div>
        </header>

        <div className="flex-1 bg-ground lg:mr-2 lg:mb-2 lg:rounded-[var(--radius-sheet)] lg:border lg:border-line">
          <main id="main" className="mx-auto w-full max-w-[1400px] px-4 pt-6 pb-16 sm:px-6 lg:px-8">
            <OrganizationGate>{children}</OrganizationGate>
          </main>
        </div>
      </div>

      <Modal open={menuOpen} onOpenChange={setMenuOpen} title="Navigation">
        <Nav onNavigate={() => setMenuOpen(false)} />
      </Modal>
    </div>
  );
}

export function ShellSkeleton() {
  return (
    <div className="min-h-dvh bg-frame lg:grid lg:grid-cols-[15rem_1fr]" aria-busy>
      <div className="hidden flex-col gap-3 p-5 lg:flex">
        <Skeleton className="h-6 w-40" />
        {Array.from({ length: 8 }, (_, i) => (
          <Skeleton key={i} className="h-10" />
        ))}
      </div>
      <div className="flex flex-col gap-4 bg-ground p-6 lg:mt-12 lg:mr-2 lg:mb-2 lg:rounded-[var(--radius-sheet)] lg:border lg:border-line">
        <Skeleton className="h-9 w-64" />
        <Skeleton className="h-40" />
      </div>
    </div>
  );
}
