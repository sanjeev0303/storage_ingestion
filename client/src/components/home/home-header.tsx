"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { Logo } from "@/components/brand/logo";
import { ThemeToggle } from "@/components/theme-toggle";
import { cn } from "@/lib/utils/cn";
import { actions, nav } from "./copy";
import { Wrap } from "./wrap";

// Fixed to the top of the page, in one of three states:
//   top   see-through over the hero, before any scrolling
//   hero  a solid ultramarine bar while the hero is still under it
//   page  a solid bar on the page's own ground once the hero has gone
type Look = "top" | "hero" | "page";

const BAR = 80;

export function HomeHeader() {
  const [look, setLook] = useState<Look>("top");

  useEffect(() => {
    const hero = document.getElementById("hero");
    let frame = 0;
    function update() {
      frame = 0;
      const y = window.scrollY;
      const heroBottom = hero ? hero.getBoundingClientRect().bottom : 0;
      setLook(y < 4 ? "top" : heroBottom > BAR ? "hero" : "page");
    }
    function onScroll() {
      if (!frame) frame = requestAnimationFrame(update);
    }
    update();
    window.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("resize", onScroll);
    return () => {
      window.removeEventListener("scroll", onScroll);
      window.removeEventListener("resize", onScroll);
      cancelAnimationFrame(frame);
    };
  }, []);

  const solid = look === "page";

  return (
    <header
      className={cn(
        "fixed inset-x-0 top-0 z-50 border-b transition-[background-color,border-color,color] duration-200 ease-out",
        look === "page" && "border-line bg-ground text-ink",
        look === "hero" && "border-[var(--on-field-line)] bg-[var(--field-ultra)] text-white",
        look === "top" && "border-transparent bg-transparent text-white",
      )}
    >
      <Wrap className="flex h-20 items-center gap-6">
        <Link
          href="/"
          aria-label="Orbidune home"
          className="-ml-1 rounded-md p-1"
          style={solid ? undefined : ({ "--logo-dune": "rgb(255 255 255 / 0.45)", "--logo-gap": "var(--field-ultra)" } as React.CSSProperties)}
        >
          <Logo />
        </Link>
        <nav aria-label="Homepage" className="ml-6 hidden md:block">
          <ul className="flex items-center gap-1">
            {nav.map((item) => (
              <li key={item.href}>
                <a
                  href={item.href}
                  className={cn(
                    "inline-flex min-h-11 items-center rounded-full px-3.5 text-[0.9375rem] font-medium transition-colors duration-150",
                    solid ? "text-muted hover:text-ink" : "text-[var(--on-field-muted)] hover:text-white",
                  )}
                >
                  {item.label}
                </a>
              </li>
            ))}
          </ul>
        </nav>
        <div className="ml-auto flex items-center gap-2">
          <ThemeToggle
            variant="ghost"
            className={cn(
              "size-11 rounded-full px-0",
              solid ? "text-ink hover:bg-faint max-sm:hidden" : "text-white hover:bg-white/12 active:bg-white/16",
            )}
          />
          <Link
            href={actions.signIn.href}
            className={cn(
              "inline-flex min-h-11 items-center rounded-full px-4 text-[0.9375rem] font-medium whitespace-nowrap transition-colors duration-150",
              solid ? "hover:bg-faint max-sm:hidden" : "hover:bg-white/12",
            )}
          >
            {actions.signIn.label}
          </Link>
          {/* On a phone the hero's own button is the action until the hero scrolls away. */}
          <span className={solid ? "block" : "hidden sm:block"}>
            <Link href={actions.primary.href} className="btn-orb min-h-11 px-5 text-[0.9375rem]">
              {actions.primary.label}
            </Link>
          </span>
        </div>
      </Wrap>
    </header>
  );
}
