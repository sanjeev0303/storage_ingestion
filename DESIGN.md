---
name: Object Storage
description: A quiet instrument for deciding whether a dataset is fit to use. Flat landing, neomorphic console.
colors:
  ground: "#DDE3E8"
  plum: "#3A0F34"
  amber: "#EAAA00"
  muted: "#6B4F6A"
  faint: "#C6C5CF"
  edge: "#836E85"
  light-edge: "#EBEFF2"
  shade-edge: "#B6B0BD"
  dark-faint: "#543151"
  dark-light-edge: "#512D4D"
  dark-shade-edge: "#20081D"
typography:
  display:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(2.75rem, 7.5vw, 5.75rem)"
    fontWeight: 600
    lineHeight: 0.98
    letterSpacing: "-0.035em"
  headline:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.75rem, 3.6vw, 2.75rem)"
    fontWeight: 600
    lineHeight: 1.25
    letterSpacing: "-0.025em"
  title:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.75rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.025em"
  panel-title:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.125rem"
    fontWeight: 600
    lineHeight: 1.5
  body:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 500
    lineHeight: 1.5
  badge:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 600
    lineHeight: 1.5
  data:
    fontFamily: "JetBrains Mono, ui-monospace, monospace"
    fontSize: "0.8125em"
    fontWeight: 400
    lineHeight: 1.5
rounded:
  sm: "2px"
  md: "4px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "20px"
  xl: "32px"
components:
  button-primary:
    backgroundColor: "{colors.plum}"
    textColor: "{colors.ground}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 16px"
  button-accent:
    backgroundColor: "{colors.amber}"
    textColor: "{colors.plum}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 16px"
  button-secondary:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.plum}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 16px"
  button-ghost-hover:
    backgroundColor: "{colors.faint}"
    textColor: "{colors.plum}"
  landing-cta:
    backgroundColor: "{colors.amber}"
    textColor: "{colors.plum}"
    height: "48px"
    padding: "0 32px"
  input:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.plum}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 12px"
  panel:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.plum}"
    rounded: "{rounded.md}"
    padding: "20px"
  state-badge-failed:
    backgroundColor: "{colors.amber}"
    textColor: "{colors.plum}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  state-badge-done:
    backgroundColor: "{colors.plum}"
    textColor: "{colors.ground}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  state-badge-quiet:
    backgroundColor: "{colors.faint}"
    textColor: "{colors.plum}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  tab-current:
    backgroundColor: "{colors.plum}"
    textColor: "{colors.ground}"
    rounded: "{rounded.sm}"
    padding: "6px 14px"
---

# Design System: Object Storage

Hex values above are the light theme, with the mixed tones resolved from the `color-mix()` rules in `client/src/app/globals.css`. The CSS custom properties are the live source; the dark-* entries are the dark-theme overrides.

## Overview

**Creative North Star: "The Quiet Instrument"**

A tool for deciding whether a dataset is fit to use. It never decorates a result; it separates what was checked from what was not, and makes "not checked" as visible as "passed". The palette (ground, plum, amber at 60/30/10) and the neomorphic console were pinned by the owner, so they are fixed inputs rather than options.

Two registers share one palette and one type family. The landing page is flat: no shadows, no cards, one plum band edge to edge. The console is neomorphic: a single ground colour, raised things lit from the top left, wells pressed in. Both stay crisp (2px and 4px corners) and dense enough for engineers reading tables and reports.

**Key Characteristics:**
- Three colours only. Everything else is a mix of ground, plum and amber.
- Meaning is carried by shape and word; hue never carries it alone.
- One ground per theme. Depth comes from a light edge and a shade edge, not from extra surface colours.
- Light and dark themes; the dark ground is the plum.

## Colors

Cool pale grey, deep aubergine plum, one warm amber. The palette is restrained and every other tone is derived.

### Primary
- **Plum** (#3A0F34): text and ink in light theme, solid actions, the full-bleed landing band, and the ground in dark theme.

### Secondary
- **Amber** (#EAAA00): one landing call to action, the current-nav marker, failed and attention fills, and the amber centre of warning marks. Always a fill with plum text on top (7.9:1). Never text on the ground (1.6:1).

### Neutral
- **Cool Ground** (#DDE3E8): page and every console surface in light theme; ink in dark theme.
- **Muted Plum** (#6B4F6A, 70% plum into ground): secondary text, descriptions, table headers.
- **Faint** (#C6C5CF, 14% plum into ground; 16% in dark): table rules, quiet badge fills, hover fill, the example report panel.
- **Form Edge** (#836E85, `--edge`, 55% plum into ground): the 1px boundary on inputs, so a control meets 3:1 against the ground.
- **Light and Shade Edges** (light #EBEFF2 / shade #B6B0BD in light theme; #512D4D / #20081D in dark): only used inside the neo shadow pairs.

### Named Rules
**The Three Colours Rule.** Ground, plum and amber, plus mixes of them. A fourth hue is a defect.

**The Amber Is A Fill Rule.** Amber appears behind plum, never as text or thin line on the ground.

**The Edge Rule.** Form controls carry `--edge` as their border so the boundary is perceivable; the neo shadow alone is not enough.

## Typography

**Display and UI Font:** Hanken Grotesk (with ui-sans-serif, system-ui)
**Mono Font:** JetBrains Mono, for ids, checksums, code and event names only

**Character:** One grotesque family, tight and confident at display size, plain at 14px. Mono marks data a person might copy and compare; it is never used for decoration.

### Hierarchy
The scale is fixed in `globals.css` (`--text-xs` to `--text-2xl`); landing display sizes are clamps.
- **Display** (600, clamp 2.75rem to 5.75rem, 0.98, -0.035em): the landing headline only.
- **Headline** (600, clamp 1.75rem to 2.75rem, tight): landing band and close headings.
- **Title** (600, 1.75rem, tracking-tight): console page titles.
- **Panel title** (600, 1.125rem): panel headings, brand name.
- **Body** (400 to 500, 0.875rem, 1.5): console default. Landing body uses 1rem to 1.125rem. Descriptions cap at prose width.
- **Label** (500, 0.8125rem): field labels, breadcrumbs, table headers (muted), hints.
- **Badge** (600, 0.75rem): state badges.
- **Data** (JetBrains Mono, 0.8125em, wraps anywhere): ids and checksums via `mono-data`.

### Named Rules
**The Mono Is Data Rule.** JetBrains Mono for ids, checksums and code, nothing else.

**The Fixed Scale Rule.** Console type comes from the seven named steps; no arbitrary pixel sizes. Only landing headings use clamps.

## Layout

Console: a 17rem left rail (nine product areas, sticky, hidden below `lg` and replaced by a modal menu), a sticky top bar on the ground (organization switcher left, theme toggle and account menu right), and a main column up to 1400px with 16/24/32px gutters. Overview is a roughly 2/3 and 1/3 composition. Page headers carry breadcrumbs, title, description and actions; sub-areas use route tabs.

Landing: 7xl container with 20/32/48px gutters, 12-column grid, a Z reading path (name and Sign in, headline left with the example report right, amber call to action bottom right of the hero, a closing statement with Sign in bottom right). The plum band takes full width and carries three left/right alternating claims with 96 to 144px vertical padding. No card rows.

Spacing rhythm follows a 4px base: 4, 8, 12, 20 (panel padding), 24 (page header gap), 32.

## Elevation & Depth

Console depth is neomorphic only: one ground with a light edge top left and a shade edge bottom right. Raised means actionable or a panel; recessed means input, current, or content inside a raised panel. Landing has no elevation at all. Dark theme keeps the same pairs recomputed from the plum.

### Shadow Vocabulary
- **neo** (`6px 6px 14px shade, -6px -6px 14px light`): panels.
- **neo-sm** (`3px 3px 7px shade, -3px -3px 7px light`): secondary and primary buttons, menus, skip link.
- **neo-in** (`inset 3px 3px 7px shade, inset -3px -3px 7px light`): recessed wells, current nav item.
- **neo-in-sm** (`inset 2px 2px 4px shade, inset -2px -2px 4px light`): inputs, tab strip, usage meter, active badge, pressed secondary button.

### Named Rules
**The Neo Pair Rule.** Only these four pairs. No other box-shadow and no drop shadow exists in the build.

**The Landing Is Flat Rule.** The landing page uses flat fills and type, never neo shadows.

## Shapes

Crisp. Every radius step in the Tailwind theme resolves to 2px or 4px: 4px for buttons, panels, inputs and nav items; 2px for badges and tabs. Landing blocks and the call to action are square. Status marks are squares (16px outcome marks, 12px state marks), never circles. Table borders are 1px faint rules with a 2px faint header rule.

## Components

### Buttons
- **Shape:** 36px high, 4px radius, 16px horizontal padding, 14px medium text, 8px icon gap.
- **Primary:** plum fill, ground text, neo-sm; pressing moves it 1px down and drops the shadow.
- **Accent:** amber fill, plum text, neo-sm; same press.
- **Secondary (default):** ground with neo-sm; pressed becomes neo-in.
- **Ghost:** transparent, faint fill on hover and press.
- **Loading and disabled:** spinner and `aria-busy`; disabled at 50% opacity and no pointer events.
- **Landing call to action:** 48px high, square, amber with plum text, lifts 2px on hover.

### Inputs / Fields
- **Style:** neo-in-sm, 1px `--edge` border, 36px high, 12px padding, muted placeholder, 4px radius.
- **Focus:** global 2px plum outline with 2px offset; amber outline in dark theme.
- **Error:** 2px plum outline plus a warning icon and message in `role="alert"`; the label always shows "(required)" in words.

### Panel
Raised neo panel, 20px padding, 18px semibold title, muted description, actions top right. Tone "in" gives a recessed well for content inside a raised panel.

### Navigation
Rail items: 4px radius, 20px Phosphor icon, 14px medium label. Current item is recessed (neo-in), uses the fill icon weight and a small amber square on the right. Others show a faint hover fill. Tabs are a recessed strip where the current tab is a solid plum chip.

### Outcome Marks and State Badges (signature)
Check outcomes are told by shape and word: passed is a filled square; partial a half-filled square; warning a ringed square with an amber centre; failed an amber square with a cross; not checked a dashed empty square. State badges add a 12px mark to a pill: failed is amber with a crossed square, warning is quiet with a ringed square, active is recessed with a play triangle, done is plum with a filled square, neutral has no mark. The word always appears.

### Tables and Meters
Tables sit in a bordered scroll region with tabular numerals, muted headers and faint rules. Usage meters state used and limit in text; the bar is a recessed track with a plum fill that turns amber with a plum outline above 90%.

### Motion
GSAP for landing choreography: hero lines rise 24px in reading order, report rows slide in one at a time, band claims reveal from their text side once on scroll. Lenis smooth scroll is a single instance driven by the GSAP ticker. UI transitions are 150ms ease-out on shadow, transform and background. Reduced motion: GSAP runs only inside a `no-preference` match media, Lenis is skipped, and CSS durations collapse to near zero. Inner scroll areas carry `data-lenis-prevent`.

### Icons
Phosphor only. Regular weight by default, fill weight for the current nav item. Icons support a word; they do not replace one.

## Do's and Don'ts

### Do:
- **Do** keep the 60/30/10 split: ground dominates, plum carries type, solid actions and the landing band, amber is the rare fill.
- **Do** build raised and recessed surfaces from the four neo pairs on the one ground.
- **Do** use `--edge` on every form control boundary.
- **Do** show status with a shape and a word together (OutcomeMark, StateBadge).
- **Do** show "Not checked" as plainly as "Passed".
- **Do** keep corners at 2px or 4px.
- **Do** use mono only for ids, checksums and code.
- **Do** respect reduced motion for both GSAP and Lenis.

### Don't:
- **Don't** use gradients, drop shadows beyond the neo pairs, glass or blur, glowing orbs, dot grids or neon.
- **Don't** use side stripes, three-card rows, or neo shadows on the landing page.
- **Don't** use em dashes or emoji anywhere in copy or UI.
- **Don't** use Lucide, or Inter, Geist or Space Grotesk. Phosphor and Hanken Grotesk are the set.
- **Don't** tell status by hue alone, or use amber as text on the ground.
- **Don't** add a privacy policy page, fake testimonials, invented customer numbers or any unbacked claim.
