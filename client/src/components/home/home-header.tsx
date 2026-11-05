import Link from "next/link";
import { ThemeToggle } from "@/components/theme-toggle";
import { nav } from "./home-copy";

export function HomeHeader() {
  return (
    <header className="field-b">
      <div className="mx-auto flex max-w-[1440px] items-center justify-between gap-6 px-4 pt-7 sm:px-10">
        <Link href="/" className="h-lead">
          Object Storage
        </Link>
        <nav aria-label="Page" className="flex items-center gap-1 sm:gap-5">
          <ul className="hidden items-center gap-6 lg:flex">
            {nav.map((item) => (
              <li key={item.href}>
                <a href={item.href} className="h-small h-link font-medium [text-decoration-thickness:1px]">
                  {item.label}
                </a>
              </li>
            ))}
          </ul>
          <ThemeToggle variant="ghost" className="size-11 px-0" />
          <Link href="/login" className="h-small inline-flex min-h-11 items-center px-2 font-medium h-link">
            Sign in
          </Link>
        </nav>
      </div>
    </header>
  );
}
