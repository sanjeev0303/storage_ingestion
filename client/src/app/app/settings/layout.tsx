import { PageHeader } from "@/components/ui/page";
import { SettingsTabs } from "@/features/organization/components/settings-tabs";

export default function SettingsLayout({ children }: LayoutProps<"/app/settings">) {
  return (
    <>
      <PageHeader
        title="Settings"
        description="Your organization, who belongs to it, and your own account."
      />
      <SettingsTabs />
      {children}
    </>
  );
}
