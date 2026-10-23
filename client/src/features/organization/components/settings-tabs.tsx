"use client";

import { TabNav } from "@/components/ui/page";
import { useSession } from "@/lib/auth/session";

const tabs = [
  { href: "/app/settings", label: "Organization" },
  { href: "/app/settings/members", label: "Members" },
  { href: "/app/settings/invitations", label: "Invitations" },
  { href: "/app/settings/roles", label: "Roles" },
  { href: "/app/settings/quota", label: "Quota" },
  { href: "/app/settings/account", label: "Account" },
];

export function SettingsTabs() {
  const { isPlatformAdmin } = useSession();
  const visible = isPlatformAdmin ? [...tabs, { href: "/app/settings/platform", label: "Platform" }] : tabs;
  return <TabNav tabs={visible} label="Settings" />;
}
