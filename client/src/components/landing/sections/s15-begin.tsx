import Link from "next/link";
import { copy } from "../copy";
import { Container, Section } from "../section";
import { BeginDialog } from "./begin-dialog";

export function Begin() {
  return (
    <Section stop="begin" tone="amber" labelledBy="begin-title" bleed className="flex min-h-[90svh] items-center">
      <Container>
        <div className="grid items-center gap-12 lg:grid-cols-12 lg:gap-8">
          <div data-reveal="left" className="lg:col-span-6">
            <h2 id="begin-title" className="t-display max-w-[11ch] max-sm:text-[13vw]">
              {copy.begin.title}
            </h2>
            <p className="t-lead mt-8 max-w-[30ch]">{copy.begin.text}</p>
            <Link
              href="/login"
              className="mt-10 inline-flex h-12 items-center justify-center bg-on-accent px-8 font-semibold text-accent transition-transform duration-150 ease-out focus-visible:outline-on-accent active:scale-[0.97]"
            >
              {copy.begin.primary}
            </Link>
          </div>
          <div className="lg:col-span-5 lg:col-start-8">
            <BeginDialog />
          </div>
        </div>
      </Container>
    </Section>
  );
}
