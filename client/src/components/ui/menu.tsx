"use client";

import * as Menu from "@radix-ui/react-dropdown-menu";
import type { ReactNode } from "react";
import { cn } from "@/lib/utils/cn";

export const DropdownMenu = Menu.Root;
export const DropdownTrigger = Menu.Trigger;

export function DropdownContent({ children, align = "end" }: { children: ReactNode; align?: "start" | "end" }) {
  return (
    <Menu.Portal>
      <Menu.Content
        align={align}
        sideOffset={6}
        className="menu neo z-50 min-w-48 p-1.5"
      >
        {children}
      </Menu.Content>
    </Menu.Portal>
  );
}

export function DropdownItem({
  className,
  ...rest
}: Menu.DropdownMenuItemProps) {
  return (
    <Menu.Item
      className={cn(
        "flex cursor-pointer items-center gap-2 rounded-sm px-3 py-2 text-base outline-none",
        "data-[highlighted]:bg-ink data-[highlighted]:text-ground data-[disabled]:opacity-50",
        className,
      )}
      {...rest}
    />
  );
}

export const DropdownLabel = ({ children }: { children: ReactNode }) => (
  <Menu.Label className="px-3 py-1.5 text-sm text-muted">{children}</Menu.Label>
);
export const DropdownSeparator = () => <Menu.Separator className="my-1 h-px bg-faint" />;
