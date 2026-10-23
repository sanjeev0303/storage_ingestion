# Homepage brief

The homepage (`src/app/page.tsx`) is 15 sections. Read this before building one. The words are in `src/components/landing/copy.ts`; use them as written and add to that file only by appending keys to your own section's block.

## The concept

**The page is the chain of custody of one dataset.** One amber token travels down a single line (the rail on the left, `custody-rail.tsx`) and hands off at 15 stops. Every stop shows what the console keeps at that hand-off, as a **working fragment of the real product** or an **animated workflow diagram**. The rail uses the product's own outcome vocabulary: stages already read are filled squares (checked), the current one is amber, the ones ahead are dashed (not checked yet).

Sections in order: 1 Hero (id `question`), 2 Problem, 3 Connect, 4 Upload, 5 Verify, 6 Ingest, 7 Catalog, 8 Snapshot, 9 Validate, 10 Report, 11 Readiness, 12 Lineage, 13 Retain, 14 Access, 15 Begin. Each already exists as a stub in `src/components/landing/sections/sNN-name.tsx` exporting the component `page.tsx` imports. Replace the stub; keep the export name and the `<Section stop=... tone=... labelledBy=...>` wrapper (change `tone` only if told).

Structure source: impeccable surface roll "chain of custody" fused with the taste skill's direction "The product performs": small, true, working fragments of the interface at real size, with real labels and sensible states, built in HTML/SVG (never grey bars). A fragment's root carries `data-ui` (inside it the product's own conventions apply: 1px control borders, the console's neomorphic edges, `Outcome`/`StateBadge` marks).

## Pinned by the owner (do not break)

- Palette only: ground `#DDE3E8` (60%), plum `#3A0F34` (30%), amber `#EAAA00` (10%). Use tokens: `bg-ground text-ink`, `bg-ink text-ground` (plum band), `bg-faint`, `bg-accent text-on-accent`, `text-muted`. Amber is a fill with plum on top, never text on the ground. SVG: `currentColor` for strokes and text, `var(--accent)` for the one accent, `var(--faint)` for quiet fills. No raw hex.
- Minimal and flat on the page. Neomorphic edges (`neo`, `neo-in`) only inside a `data-ui` fragment that reproduces the console. No gradients, no drop shadows on resting elements, no glass, no blobs or orbs, no dot grids, no neon, no rainbow colouring, no emoji, no sparkle icons, no Lucide (Phosphor only, rarely), no checkmark bullets, no coloured left stripes, no rows of three identical cards, no terminal windows, no fake testimonials or invented numbers, no privacy policy. No em dashes anywhere. No "it is not X, it is Y" copy. No eyebrow labels, no 01/02/03 numbering, no tiny uppercase labels.
- Corners are crisp: 0, 2 or 4px. One button shape: square, `h-12`, `px-8`. Fonts: Hanken Grotesk (everything), JetBrains Mono only for ids, paths, checksums (`mono` class in SVG, `mono-data` in HTML).
- Layout: Z pattern and rule of thirds; alternate text-left/diagram-right and the reverse; every section's heading in the first third of its block. Plum (30%) sections: 5, 7, 12, 14 (and part of 6). Amber: section 15 only, plus small accents.

## Type (five steps, nothing under 13px rendered)

`t-display` (h1 only), `t-h2` (section headings, two lines at most at every width), `t-lead`, `t-body` (17px), `t-small` (13px). No other sizes in HTML. Inside SVG, text uses viewBox units; keep every diagram's viewBox about 420 units wide so 15 to 16 units render at roughly 14 to 15px on a phone; never put text under 13 units.

## Diagrams

Use `src/components/landing/diagram-kit.tsx`:

- `<DiagramFigure label="..." caption?>` wraps every diagram. The SVG is a picture; `label` is its text alternative (the labels are in `copy.ts`).
- `useDiagramTimeline(ref, (tl, root) => { ... })` builds one paused GSAP timeline that plays only while the diagram is on screen (ScrollTrigger inside the hook), loops with a pause, and is skipped for `prefers-reduced-motion`. **The markup must already be the finished picture** (everything visible, no hidden initial state in CSS): the timeline uses `tl.from(...)`, `tl.fromTo(...)`, `DrawSVGPlugin`, `MotionPathPlugin` to animate toward it. Without script or with reduced motion the reader sees the complete diagram. Never leave content hidden if script fails.
- `Mark` (the five outcome shapes in SVG) and `Token` (the amber token).
- Animate only `transform`, `opacity`, `strokeDashoffset`/`drawSVG`. Prefer `x`, `y`, `scale`, `autoAlpha`. No layout properties. Stagger 0.05 to 0.09 s. Entrances use `power2.out` / `power3.out`. One idea per diagram; the motion must explain the workflow, not decorate it.
- Diagram text must name real states and words from the product (`QUARANTINED`, `PROCESSING`, `AVAILABLE`, `REJECTED`, `Pending`, `Active`, `FINALIZED`, `PASSED` ... as in `copy.ts`).
- Mobile first: diagrams are vertical (portrait viewBox around 420 wide) so they fill a phone and sit in one column on desktop. Only section 5 uses a horizontal variant on `md` and up and a vertical one below; render both and hide one with Tailwind (`md:hidden` / `hidden md:block`), each with its own timeline.
- Tone: in a plum section the wrapper sets `text-ground`, so `currentColor` is the light colour; amber stays amber. Test both light and dark theme (in dark, `ink` and `ground` swap, so a plum band becomes a light band: use `currentColor` and tokens, never fixed colours).

## Motion rules

GSAP only (`useGSAP` from `@gsap/react`, scoped, `gsap.matchMedia` for reduced motion; Lenis smooth scroll is already mounted at the root). Section entrances use `data-reveal="left" | "right" | "fade"` (handled once by `LandingMotion`) or `data-hero`. Interaction feedback: press scale 0.97 on buttons (already in `ButtonLink`/`Button`; the homepage's own `a` buttons add `active:scale-[0.97]`), 150 to 250 ms. Keyboard interactions do not animate beyond a state change. One authored moment per section.

## Accessibility

Real headings (`<h2 id="{stop}-title">`, visible, two lines or fewer), landmarks, labelled controls, visible focus (already themed), keyboard operation for every interactive fragment (radio groups, sliders, tabs, listboxes follow the WAI-ARIA pattern), `aria-live="polite"` where a toggle changes a result, information never by colour alone (marks plus words), text contrast 4.5:1, touch targets at least 44px, nothing hidden from screen readers that conveys meaning, SVG diagrams exposed through their `label`.

## Performance

Diagrams pause off screen; no per-frame React state; no new dependencies; images none needed (the fragments are the pictures). Interactive fragments are client components; static sections stay server components where they can.

## Verification each agent must run

`npx tsc --noEmit`, `npx eslint <your files>`, `npx vitest run <your tests>`; write focused tests for behaviour (a toggle changes the report, a filter narrows a list, a slider maps to the right stage, tabs switch, keyboard works) plus an axe check on each interactive fragment. Then look at your section for real: the dev server runs at http://localhost:3100 (do not start another). With Playwright (`import { chromium } from "@playwright/test"`, `channel: "chrome"`, `reducedMotion: "reduce"` for stills) screenshot `#<stop-id>` at 1440x900 and 390x844, in light and dark theme (`colorScheme`), Read the images and fix what is wrong, once. Check at 1440, 1280 and 390 widths for overflow, clipped text and diagrams that become unreadable. Report what you saw. Do not run git add, commit or push; do not install packages; do not edit files outside your sections, your diagram files and your tests (shared files: only additive, and say so).
