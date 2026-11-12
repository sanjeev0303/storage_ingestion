import Link from "next/link";
import { Logo } from "@/components/brand/logo";
import { OrbitDunes } from "@/components/brand/orbit-dunes";
import { ThemeToggle } from "@/components/theme-toggle";

// Sign-in, registration and recovery: the form on the sheet, Orbidune's claim
// on the ultramarine field beside it on wide screens.
export default function AuthLayout({ children }: LayoutProps<"/">) {
  return (
    <div className="grid min-h-dvh lg:grid-cols-[minmax(0,5fr)_minmax(0,7fr)]">
      <aside className="field-ultra relative hidden grid-rows-[auto_1fr_auto] overflow-hidden lg:grid">
        <Link href="/" aria-label="Orbidune home" className="m-10 w-fit rounded-md xl:m-14">
          <Logo />
        </Link>
        <div className="self-center px-10 xl:px-14">
          <p className="t-h2 max-w-[14ch]">Verified dataset storage for ML teams</p>
          <p className="t-body mt-5 max-w-[30rem] text-[var(--on-field-muted)]">
            Every upload is hashed and scanned before anyone can read it, and every report says which checks ran.
          </p>
        </div>
        <OrbitDunes className="h-[38vh] max-h-[360px] min-h-[220px]" />
      </aside>
      <div className="mx-auto flex min-h-dvh w-full max-w-3xl flex-col px-5 sm:px-8">
        <header className="flex items-center justify-between py-6">
          <Link href="/" aria-label="Orbidune home" className="rounded-md lg:invisible">
            <Logo />
          </Link>
          <ThemeToggle />
        </header>
        <main id="main" className="flex flex-1 items-center justify-center pb-16">
          {children}
        </main>
      </div>
    </div>
  );
}
