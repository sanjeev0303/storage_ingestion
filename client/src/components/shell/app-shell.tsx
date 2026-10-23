"use client";

import { CaretDown, List, SignOut, UserCircle } from "@phosphor-icons/react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState, type ReactNode } from "react";
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
                        "flex min-h-11 items-center gap-3 rounded-md px-3 text-base font-medium transition-shadow",
                        current ? "neo-in text-ink" : "hover:bg-faint",
                      )}
                    >
                      <Icon
                        aria-hidden
                        weight={current ? "fill" : "regular"}
                        className={cn("size-5 shrink-0", current && "text-ink")}
                      />
                      {item.label}
                      {current && <span aria-hidden className="ml-auto size-2 bg-accent" />}
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
    <div className="min-h-dvh lg:grid lg:grid-cols-[17rem_1fr]">
      <aside className="hidden lg:block">
        <div className="sticky top-0 flex h-dvh flex-col gap-6 overflow-y-auto p-5" data-lenis-prevent>
          <Link href="/app/overview" className="px-1 text-lg font-bold tracking-tight">
            Object Storage
          </Link>
          <Nav />
        </div>
      </aside>

      <div className="min-w-0">
        <header className="sticky top-0 z-30 flex items-center gap-3 bg-ground px-4 py-3 sm:px-6 lg:px-8">
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

        <main id="main" className="mx-auto w-full max-w-[1400px] px-4 pt-4 pb-16 sm:px-6 lg:px-8">
          <OrganizationGate>{children}</OrganizationGate>
        </main>
      </div>

      <Modal open={menuOpen} onOpenChange={setMenuOpen} title="Navigation">
        <Nav onNavigate={() => setMenuOpen(false)} />
      </Modal>
    </div>
  );
}

export function ShellSkeleton() {
  return (
    <div className="min-h-dvh lg:grid lg:grid-cols-[17rem_1fr]" aria-busy>
      <div className="hidden flex-col gap-3 p-5 lg:flex">
        <Skeleton className="h-6 w-40" />
        {Array.from({ length: 8 }, (_, i) => (
          <Skeleton key={i} className="h-10" />
        ))}
      </div>
      <div className="flex flex-col gap-4 p-6">
        <Skeleton className="h-9 w-64" />
        <Skeleton className="h-40" />
      </div>
    </div>
  );
}
