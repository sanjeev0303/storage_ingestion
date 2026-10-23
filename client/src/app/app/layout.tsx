import { AppShell, ShellSkeleton } from "@/components/shell/app-shell";
import { SessionProvider } from "@/lib/auth/session";

export default function ConsoleLayout({ children }: LayoutProps<"/app">) {
  return (
    <SessionProvider fallback={<ShellSkeleton />}>
      <AppShell>{children}</AppShell>
    </SessionProvider>
  );
}
