import Link from "next/link";
import { CustodyRail } from "@/components/landing/custody-rail";
import { STOPS } from "@/components/landing/copy";
import { LandingMotion } from "@/components/landing/motion";
import { Container } from "@/components/landing/section";
import { Access } from "@/components/landing/sections/s14-access";
import { Begin } from "@/components/landing/sections/s15-begin";
import { Catalog } from "@/components/landing/sections/s07-catalog";
import { Connect } from "@/components/landing/sections/s03-connect";
import { Hero } from "@/components/landing/sections/s01-hero";
import { Ingest } from "@/components/landing/sections/s06-ingest";
import { Lineage } from "@/components/landing/sections/s12-lineage";
import { Problem } from "@/components/landing/sections/s02-problem";
import { Readiness } from "@/components/landing/sections/s11-readiness";
import { Report } from "@/components/landing/sections/s10-report";
import { Retain } from "@/components/landing/sections/s13-retain";
import { Snapshot } from "@/components/landing/sections/s08-snapshot";
import { Upload } from "@/components/landing/sections/s04-upload";
import { Validate } from "@/components/landing/sections/s09-validate";
import { Verify } from "@/components/landing/sections/s05-verify";
import { ThemeToggle } from "@/components/theme-toggle";

// Fifteen hand-offs in the custody record of one dataset. The rail on the left
// follows the reader through them; each section owns the diagram or working
// fragment that shows what the console keeps at that stop.
export default function Landing() {
  return (
    <LandingMotion>
      <CustodyRail stops={STOPS} />

      <header className="bg-ground text-ink">
        <Container className="flex items-center justify-between py-6">
          <span className="text-lg font-bold tracking-tight">Object Storage</span>
          <nav aria-label="Account" className="flex items-center gap-3">
            <ThemeToggle variant="ghost" className="size-11 px-0" />
            <Link href="/register" className="inline-flex min-h-11 items-center px-3 text-md font-semibold underline underline-offset-4">
              Create account
            </Link>
            <Link href="/login" className="inline-flex min-h-11 items-center px-3 text-md font-semibold underline underline-offset-4">
              Sign in
            </Link>
          </nav>
        </Container>
      </header>

      <main id="main">
        <Hero />
        <Problem />
        <Connect />
        <Upload />
        <Verify />
        <Ingest />
        <Catalog />
        <Snapshot />
        <Validate />
        <Report />
        <Readiness />
        <Lineage />
        <Retain />
        <Access />
        <Begin />
      </main>

      <footer className="bg-ground text-ink">
        <Container className="flex flex-wrap items-center justify-between gap-4 py-8">
          <span className="t-small">Object Storage</span>
          <Link href="/login" className="t-small inline-flex min-h-11 items-center underline underline-offset-4">
            Sign in
          </Link>
        </Container>
      </footer>
    </LandingMotion>
  );
}
