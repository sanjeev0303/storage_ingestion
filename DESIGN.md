---
name: Object Storage
description: Storage that checks every byte first. Two worlds - a strip-board console and a specimen-accession homepage.
colors:
  frame: "#E9ECE8"
  ground: "#FFFFFF"
  ink: "#10161B"
  accent: "#FF7A1F"
  on-accent: "#10161B"
  muted: "#494E52"
  faint: "#ECECED"
  well: "#F3F5F2"
  line: "#E5E5E6"
  control-line: "#CACCCD"
  edge: "#7C7F82"
  dark-frame: "#0B1013"
  dark-ground: "#141B20"
  dark-ink: "#E4E9E5"
  dark-muted: "#BAC0BE"
  dark-faint: "#293034"
  dark-well: "#10161A"
  dark-line: "#31383C"
  dark-edge: "#868C8C"
  home-orange: "#FF7A1F"
  home-night: "#050F16"
  home-white: "#FFFFFF"
  home-dark-ink: "#F3EBE3"
  home-tint-1: "#FFE6D4"
  home-tint-2: "#FFC9A3"
  home-tint-3: "#FFAD75"
  home-rust: "#9A3D0A"
  home-rust-ink: "#FFF4EA"
  home-dark-tint-1: "#13242F"
  home-dark-tint-2: "#1B2F3B"
  home-dark-tint-3: "#233946"
typography:
  title:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.75rem"
    fontWeight: 600
    lineHeight: 1.5
    letterSpacing: "-0.025em"
  bay-title:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.375rem"
    fontWeight: 600
    lineHeight: 1.5
  panel-title:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.125rem"
    fontWeight: 600
    lineHeight: 1.5
  strip-name:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1rem"
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
    fontSize: "0.8125rem"
    fontWeight: 600
    lineHeight: 1.5
  data:
    fontFamily: "JetBrains Mono, ui-monospace, monospace"
    fontSize: "max(0.8125em, 13px)"
    fontWeight: 400
    lineHeight: 1.5
  home-display:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(3.25rem, 5.2vw, 5rem)"
    fontWeight: 500
    lineHeight: 0.95
    letterSpacing: "-0.04em"
  home-head:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(2rem, 1.3rem + 3.2vw, 4rem)"
    fontWeight: 500
    lineHeight: 0.98
    letterSpacing: "-0.04em"
  home-lead:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.25rem, 1.1rem + 0.7vw, 1.625rem)"
    fontWeight: 500
    lineHeight: 1.25
    letterSpacing: "-0.01em"
  home-body:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.0625rem, 1rem + 0.25vw, 1.1875rem)"
    fontWeight: 400
    lineHeight: 1.45
    letterSpacing: "-0.005em"
  home-small:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(0.8125rem, 0.79rem + 0.1vw, 0.875rem)"
    fontWeight: 400
    lineHeight: 1.4
  home-poster:
    fontFamily: "Hanken Grotesk, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(6rem, 36vw, 34rem)"
    fontWeight: 500
    lineHeight: 0.8
    letterSpacing: "-0.05em"
rounded:
  sm: "2px"
  md: "4px"
  sheet: "12px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  strip: "16px"
  lg: "20px"
  xl: "32px"
  rack: "40px"
  seam: "6px"
components:
  frame:
    backgroundColor: "{colors.frame}"
    textColor: "{colors.ink}"
  sheet:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.ink}"
    rounded: "{rounded.sheet}"
  button-primary:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.ground}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 16px"
  button-accent:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.on-accent}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 16px"
  button-secondary:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 16px"
  button-ghost-hover:
    backgroundColor: "{colors.faint}"
    textColor: "{colors.ink}"
  input:
    backgroundColor: "{colors.well}"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 12px"
  panel:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    padding: "20px"
  nav-current:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    height: "44px"
    padding: "0 12px"
  strip:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    padding: "10px 16px"
  strip-cell-failed:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.on-accent}"
    width: "40px"
  strip-cell-active:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.ground}"
    width: "40px"
  strip-cell-pale:
    backgroundColor: "{colors.faint}"
    textColor: "{colors.ink}"
    width: "40px"
  state-badge-failed:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.on-accent}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  state-badge-active:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.ground}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  state-badge-quiet:
    backgroundColor: "{colors.faint}"
    textColor: "{colors.ink}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  tab-current:
    textColor: "{colors.ink}"
    height: 44px
    padding: "0 12px"
  home-button:
    backgroundColor: "{colors.home-night}"
    textColor: "{colors.home-orange}"
    height: "48px"
    padding: "0 22px 0 26px"
  home-button-dark:
    backgroundColor: "{colors.home-orange}"
    textColor: "{colors.home-night}"
    height: "48px"
    padding: "0 22px 0 26px"
  home-chip:
    textColor: "{colors.home-night}"
    typography: "{typography.home-small}"
    height: "44px"
    padding: "0 14px 0 16px"
  home-chip-chosen:
    backgroundColor: "{colors.home-night}"
    textColor: "{colors.home-orange}"
    height: "44px"
    padding: "0 14px 0 16px"
  home-stage:
    backgroundColor: "{colors.home-orange}"
    textColor: "{colors.home-night}"
  home-slip:
    backgroundColor: "{colors.home-white}"
    textColor: "{colors.home-night}"
    padding: "28px 32px 40px"
  home-start-band:
    backgroundColor: "{colors.home-night}"
    textColor: "{colors.home-orange}"
    typography: "{typography.home-poster}"
  home-start-band-hover:
    backgroundColor: "{colors.home-orange}"
    textColor: "{colors.home-night}"
---

# Design System: Object Storage

Hex values above are resolved from `client/src/app/globals.css`, which stays the live source. Console tokens are the light theme with `color-mix()` tones resolved; dark-* entries are the console's dark overrides (`edge` is not overridden, so `dark-edge` is the same 55% mix recomputed on the dark sheet). Every `home-*` token is scoped to the `.home` block at the end of `globals.css` (`--h-*` custom properties) and exists only on the homepage.

**Two surfaces, two worlds.** The signed-in console (`client/src/app/app/**` and `client/src/components/**` outside `components/home`) is "The Strip Board": a tinted frame holding the sidebar and top bar, a white work sheet, flat hairline surfaces, blue-black ink, and specimen orange only as a fill under ink on what needs the user. The homepage (`client/src/app/page.tsx`, `client/src/components/home/**`) is "The Accession Label": drenched specimen orange, blue-black ink and white, flat, with one cut-corner motif. They share Hanken Grotesk, JetBrains Mono for data, the outcome-mark shapes, and the orange hex itself (the console's `--accent` and the homepage's `--h-orange` are both #FF7A1F); the console spends it on small cells and pills, the homepage on whole fields. Neither world borrows the other's surface model or corner language: no orange fields or cut corners in the console, no frame, sheet or strips on the homepage. The homepage reuses the interactive flow diagrams from `client/src/components/landing/` (`flow-kit.tsx`, `diagram-kit.tsx`, `diagrams/*-flow.tsx`); inside `.home` they read the homepage palette through CSS variables. The rest of that folder (the old flat plum landing, its custody rail and sections) is no longer rendered and is not part of either system.

## Overview

**Creative North Star (console): "The Strip Board"**

A tool for deciding whether a dataset is fit to use, laid out like an air-traffic strip board. Every job, dataset, connection and dead letter is a strip racked by its state, and what needs the user sits in the front bay as the biggest block. It never decorates a result; it separates what was checked from what was not, and makes "not checked" as visible as "passed". One tinted frame holds the sidebar and the top bar as a single L; the work sits on a white sheet inset from the window's right and bottom edges. Surfaces are flat and told apart by tone and a 1px hairline, never by shadow. Ink does the work; orange is rare and always means "look here".

**Creative North Star (homepage): "The Accession Label"**

Datasets are handled the way a lab accessions specimens: received, tested and labelled before release, and the label says which tests were never run. Colour owns whole regions: full-width orange, white and blue-black fields meet at hard horizontal seams, with no gradients, cards or shadows. One motif comes from the subject and does all the shaping work: the accession label, a rectangle with its top-left corner cut, plus its barcode. It is the hero stage, every button, chip and segment, the readiness spines, the connection label, the closing Start band and the footer name. Product fragments (`data-ui`) are real console pieces enlarged in em, cropped by their tile edges.

**Key Characteristics:**
- Console: frame, sheet and ink, with every other tone mixed from ink and the sheet; specimen orange only as a fill with ink on top; flat, with depth told by the frame-to-sheet step and hairlines.
- Homepage: orange, blue-black and white fields with three orange tints; flat; the cut corner is the only shape idea.
- Both: meaning is carried by shape and word, never hue alone; "Not checked" is shown as plainly as "Passed".
- Both have light and dark themes. The console's dark frame and sheet are near-black blue-greys and its orange keeps ink on it; the homepage's dark rest field is the blue-black, and its orange fields stay orange.

## Colors

### Console
A grey-green frame, a white sheet, blue-black ink, and one specimen orange that marks only what needs the user. Everything else is ink mixed toward the sheet.

- **Board Frame** (`--frame`): the L that holds the sidebar and the top bar, with no rule between them; the theme colour in the browser chrome. Never a panel fill on the sheet.
- **Sheet** (`--ground`): the work surface and every panel, strip, menu and dialog on it. White in light theme.
- **Blue-Black Ink** (`--ink`): text, the primary button, the current tab, the in-motion strip cell and badge, meter fills, and the light-theme focus outline.
- **Specimen Orange** (`--accent`, with `--on-accent` ink on top at 7:1): the failed strip cell, the failed badge, the "need you" count on the board line when above zero, the accent button for decisive or destructive actions, the orange core of stalled and warning marks, meters above 90%, text selection, and the dark-theme focus outline (about 6.7:1 on the dark sheet). Never text on the light sheet (2.6:1), never chrome, never the primary button.
- **Muted Ink** (76% ink into the sheet; 80% in dark): secondary text, descriptions, nav items at rest, table headers, timestamps. 4.5:1 or better on the sheet, the frame and the faint fill in both themes.
- **Faint** (8% ink; 10% in dark): pale strip cells, quiet badges, the zero-count pill, ghost hover, table rules.
- **Well** (55% frame into the sheet; 50% in dark): one tone step toward the frame inside the sheet: inputs, meter tracks, skeletons, empty and error states.
- **Hairline** (`--line`, 11% ink; 14% in dark): the 1px border on the sheet, panels, strips, the current nav item, and the activity log's rules. About 1.25:1; it separates, it does not carry meaning.
- **Control Line** (22% ink): the resting border of secondary buttons.
- **Form Edge** (`--edge`, 55% ink): input boundaries, the hover border of secondary buttons, and the dashed empty slot; 3:1 against the sheet.

### Homepage
Saturated specimen-label orange against blue-black and white. Orange is a field, not an accent.

- **Specimen Orange** (primary, `--h-orange`): the drenched field (`field-a`), the hero stage, the footer name label, chosen chips on dark grounds, and button ink in light theme. It stays orange in both themes; it is the motif at large scale (`--h-mark`).
- **Blue-Black** (`--h-night`): ink in light theme, the action fill in light theme, the `field-night` band and dark tiles, the Start band, and the rest field in dark theme. Orange on blue-black and blue-black on orange are the two loud pairings.
- **White** (`--h-b` in light): the rest field (`field-b`), the accession slip, the connection label.
- **Warm Paper Ink** (`--h-ink` in dark): body ink on the blue-black rest field.
- **Orange Tints 1-3** (`--h-tint-1..3`): quieter label fills (feature tiles, readiness cards and spines, the second stage). In dark theme they become the three blue-black tints.
- **Rust and Rust Ink**: the fourth stage cell only, the step between orange and blue-black in the stage bar.
- Chip rest fills and dividers are `currentColor` at 14%, so they read on every field.

### Named Rules
**The Mixed From Ink Rule (console).** Frame, sheet, ink and orange are the only declared colours; every other console tone is `color-mix()` of ink with the sheet (or the frame with the sheet). A fifth hue, or a cool grey that is not ink, is a defect.

**The Orange Needs You Rule (console).** Orange is a fill with ink on top, and only on what needs the user. It is never text or a thin line on the light sheet, never navigation chrome, never the primary action. On the board, failed is the only fully orange strip cell.

**The Edge Rule (console).** Form controls carry `--edge` as their border so the boundary meets 3:1; the hairline alone is not enough for a control.

**The Field Owns The Region Rule (homepage).** A colour fills a whole full-width section or a whole label, edge to edge. Sections change colour only at hard horizontal seams; no gradients, no colour washes, no accent specks.

**The Orange Stays Rule (homepage).** Dark theme flips the rest field and the action, never the orange fields. The accession slip stays white with blue-black ink in both themes because it is a printed label sitting on the orange stage.

## Typography

**Font:** Hanken Grotesk (with ui-sans-serif, system-ui) on both surfaces.
**Mono:** JetBrains Mono, for IDs, keys, hashes, checksums and event names only (`mono-data`).

**Character:** One grotesque family on both surfaces; it was kept for the console over IBM Plex Sans so the console and the homepage share one voice. In the console it runs at 400, 500 and 600 with tabular figures, semibold for titles and strip names, plain at 14px. On the homepage it is medium weight throughout, set tight (-0.04em) at the large steps, so the type reads like a printed label rather than an editorial page.

### Console hierarchy
The scale is fixed in `globals.css` (`--text-xs` to `--text-2xl`).
- **Title** (600, 1.75rem, tracking-tight): console page titles.
- **Bay title** (600, 1.375rem): the front-bay heading on the overview ("Needs you"), the one rack that outranks the rest.
- **Panel title** (600, 1.125rem): rack headings, panel headings, the brand name.
- **Strip name** (600, 1rem): front-bay strip names and sub-rack headings. Ordinary strips set their name at 600 in the body size.
- **Body** (400 to 500, 0.875rem, 1.5): console default, nav items, buttons. Descriptions cap at prose width.
- **Label** (500, 0.8125rem): field labels, breadcrumbs, table headers (muted), strip details and times, rack notes, nav group names.
- **Badge** (600, 0.8125rem): state badges and the board-line count pill.
- **Data** (JetBrains Mono, 0.8125em with a 13px floor, wraps anywhere, `translate="no"`): job IDs, permission keys, event names.

### Homepage hierarchy
Five fluid steps on `.home` (`h-display`, `h-head`, `h-lead`, `h-body`, `h-small`) plus one poster word. Nothing renders under 13px.
- **Display**: the hero headline on two lines, and the footer name label. Balanced wrap.
- **Head**: section headings, and the accession number on the slip.
- **Lead**: the wordmark, stage names, provider names, readiness card titles.
- **Body**: sentences, capped at 22ch to 46ch.
- **Small**: links, legends, chips, slip rows, footer; set at 500 when it labels something.
- **Poster**: the single word in the Start band, bottom-aligned, the only size outside the five steps.

### Named Rules
**The Mono Is Data Rule.** JetBrains Mono for IDs, keys, hashes, checksums and event names, nothing else, on both surfaces.

**The Fixed Scale Rule (console).** Console type comes from the named steps; no arbitrary pixel sizes.

**The Five Steps Rule (homepage).** Homepage type uses only the five `h-*` steps and the one poster word. The exception is a `data-ui` product fragment, which may size its own controls in em so it scales as one piece.

## Layout

**Console:** a tinted frame in an L: a 15rem sidebar (brand, then nav groups; sticky, full height, hidden below `lg` and replaced by a modal menu) and a sticky top bar on the frame (48px at `lg`, 56px below; organization switcher left, command palette, theme toggle and account menu right). The work sheet fills the rest, inset 8px from the window's right and bottom edges at `lg` with a 1px hairline and 12px corners; below `lg` the sheet is the whole screen. Inside it the main column runs up to 1400px with 16/24/32px gutters, 24px above and 64px below. Page headers carry breadcrumbs, title, description and actions; sub-areas use route tabs. The overview is a strip board on a 2/3 and 1/3 grid: the board line under the title, the front bay ("Needs you") then "In motion and ready" in the wide column, Storage, Quota and dead letters in the narrow one, and the activity log full width beneath. Racks sit 40px apart, strips 8px apart, rack header 12px above its strips. Spacing follows a 4px base: 4, 8, 12, 16 (strip padding), 20 (panel padding), 24 (column gap), 32, 40. Every other console screen keeps its own layout and only takes these tokens.

**Homepage:** full-width fields stacked at hard seams, content in a 1440px container with 16px gutters (40px from `sm`) on a 12-column grid from `lg`. Sections are generous: 96px top and bottom, 128 to 144px at `lg`. The hero puts the statement in columns 1 to 5 and drops the orange stage into columns 5 to 12 from about a third of the way down, bleeding off the right and bottom edges (the page clips horizontal overflow). Labels and cells that sit side by side are separated by a 6px seam (stage bar, feature board, depth grid, depth segments, readiness spines), never by borders or gutters of white space alone. The feature board is a dense 12-column bento with no empty cell, two columns on phones. Below `lg`, the readiness spines become stacked cards and the depth control stacks vertically. Every interactive target is at least 44px.

## Elevation & Depth

**Console:** flat. No box-shadow anywhere at rest or on interaction. Depth has three levels and stops there: the frame, the sheet on it, and strips or panels on the sheet, each step told by tone and a 1px hairline. A well (inputs, meter track, empty and error states) is one tone step back toward the frame, not a pressed shadow. Menus, dialogs and toasts are sheet-coloured panels with a hairline over a dimmed overlay. The `neo`, `neo-sm`, `neo-in` and `neo-in-sm` utility names survive only so every screen took the new look; they are now flat surfaces.

**Homepage:** flat. No shadows, no blur, no layering illusions. Depth is told by field colour and by the stage bleeding past the viewport edge. The only `box-shadow` on the homepage is a 3px inset ring that marks keyboard focus on cut-corner controls, because `clip-path` would cut an outline.

### Surface Vocabulary (console)
- **neo**: sheet fill, 1px hairline, 4px radius. Panels, menus, dialogs, toasts.
- **neo-sm**: sheet fill, 1px control line, 4px radius. Secondary buttons, the skip link.
- **neo-in**: well fill, 1px hairline, 4px radius. Recessed panels, empty and error states.
- **neo-in-sm**: well fill, no border, 4px radius. Inputs (which add the edge), meter tracks, skeletons.

### Named Rules
**The Three Levels Rule (console).** Frame, sheet, strip, and nothing deeper. On the overview the strips are the cards: racks are not wrapped in panels, and a strip never holds a nested card.

**The No Shadow Rule (console).** Surfaces separate by tone and hairline only. No box-shadow, drop shadow or neomorphic pair in the console.

**The Flat Field Rule (homepage).** The homepage uses flat fills, type and the cut corner, never neo shadows or drop shadows. The inset focus ring is the one allowed shadow.

## Shapes

**Console:** crisp. Every radius step resolves to 2px or 4px: 4px for buttons, panels, strips, inputs and nav items; 2px for badges, tabs and the count pill. The sheet alone is 12px. Status marks are squares (16px outcome marks, 12px state marks in badges, 16px state marks in strip cells), plus the play triangle for in motion; never circles. Table borders are 1px faint rules with a 2px faint header rule. An empty slot is a dashed edge-colour border so it never reads as a strip.

**Homepage:** no radius anywhere. Square corners everywhere, except the one cut: the accession label is a four-point `clip-path` polygon with its top-left corner chamfered at 45 degrees (`--cut`). The cut scales with the label it belongs to:
- controls: chip 8px, button 10px, depth segment 0.5em;
- cards: readiness spine and card 16px, connection label 40px, accession slip 44px, fragment label 1.4em;
- field scale: footer name label 56px (120px at `lg`), hero stage 72px (128px at `lg`), Start band 72px (160px at `lg`).

The barcode is the motif's second part: vertical bars generated deterministically from a string, so the same text always draws the same bars. It appears on the slip beside the hash it encodes, in the audit fragment, under the footer line, and once as a full-width blue-black band that splits the page. It is always decorative (`aria-hidden`) and the text it stands for is printed nearby.

### Named Rules
**The One Soft Corner Rule (console).** Only the sheet takes 12px. Everything on it stays at 2px or 4px.

**The One Cut Rule (homepage).** Only the top-left corner is ever cut, always at 45 degrees, always as a four-point polygon. No other corner, no rounding, no second shape idea.

## Components

### Console

#### Buttons
- **Shape:** 36px high, 4px radius, 16px horizontal padding, 14px medium text, 8px icon gap. Transform, background and border change over 150ms on the console ease; every variant scales to 0.97 on press.
- **Primary:** ink fill, sheet text. At most one per view (on the overview, "Upload files").
- **Accent:** orange fill, ink text. Reserved for decisive or destructive confirmations (delete, archive, cancel, erase, decide).
- **Secondary (default):** sheet fill with the 22% control line; hover darkens the border to the edge, press adds the faint fill.
- **Ghost:** transparent, faint fill on hover and press.
- **Loading and disabled:** spinner and `aria-busy`; disabled at 50% opacity and no pointer events.

#### Inputs / Fields
- **Style:** well fill, 1px `--edge` border, 36px high, 12px padding, muted placeholder, 4px radius. Checkboxes take the ink as their accent colour.
- **Focus:** global 2px ink outline with 2px offset; orange outline in dark theme.
- **Error:** a warning icon and message in `role="alert"`; the label always shows "(required)" in words.

#### Panel
Flat sheet panel with a hairline, 20px padding, 18px semibold title, muted description, actions top right. Tone "in" gives a well for content inside a panel. Used on the other console screens; the overview has none.

#### Navigation
Sidebar items sit on the frame: 44px high, 4px radius, 20px Phosphor icon, 14px medium label, muted at rest and ink on hover. The current item is a small piece of sheet (sheet fill, hairline, ink text, fill icon weight). Group names are 13px semibold muted text above their items. Tabs sit on a hairline rule: muted 14px medium labels, 44px high, and the current tab in ink with a 2px ink underline laid over the rule. A row too long for the screen scrolls sideways, and the current tab is scrolled into view.

#### Strip (signature)
The overview's unit. A strip is a sheet row with a 1px hairline and 4px corners, split into a 40px state cell on the left and the body. The cell is a filled block carrying a 16px drawn mark, never a coloured border:
- **Failed:** orange cell, ink crossed square. The only fully orange cell.
- **Stalled or partly published:** pale (faint) cell, ink-ringed square with an orange core, so failed stays the loudest.
- **In motion:** ink cell, play triangle in sheet colour.
- **Done:** pale cell, filled ink square. The filled square means a real pass (for a connection, a test that succeeded).
- **Waiting or never tested:** pale cell with muted ink, hollow square.

The body holds the name (semibold; mono for job IDs and event names; a link underlined in the hairline colour that turns ink on hover) with a muted detail line under it, and on the right the state word in semibold over a muted tabular time. Front-bay strips are larger: 1rem names and 16px vertical padding against 10px.

#### Racks, Board Line and Empty Slot
- **Rack header:** heading (bay title for "Needs you", panel title elsewhere), optional muted note, and an underlined text link on the right. Sub-racks inside the bay use the strip-name size.
- **Board line:** the description under the overview title counts the racks: a 2px-radius pill that is orange with ink ("N need you") above zero and faint ("Nothing needs you") at zero, then plain tabular counts for in motion, ready and completed.
- **Short list:** a rack shows four strips and a ghost "Show N more" button.
- **Empty slot:** a dashed edge-colour box with muted text saying what would appear and why it is empty. Permission gaps name the missing key in mono.

#### Activity Log
A plain list between hairline rules, newest first, time on the left: muted tabular time, the event name in mono with "by" and the actor muted, and a state badge on the right. No panel around it.

#### Outcome Marks and State Badges
Check outcomes are told by shape and word: passed is a filled square; partial a half-filled square; warning a ringed square with an orange centre; failed an orange square with a cross; not checked a dashed empty square. State badges add a 12px mark to a 2px-radius pill: failed is orange with a crossed square, warning is faint with a ringed square and an orange core, active is ink with a play triangle, done is faint with a filled square, neutral is faint with no mark. The word always appears.

#### Tables and Meters
Tables sit in a faint-bordered scroll region with tabular numerals, muted semibold headers and faint rules. Usage meters state used and limit in text; the bar is a well track with an ink fill that turns orange with a 1px ink outline above 90%.

#### Icons
Phosphor only. Regular weight by default, fill weight for the current nav item. Icons support a word; they do not replace one.

#### Motion
- **UI transitions:** 150ms on `cubic-bezier(0.23, 1, 0.32, 1)` (the console's `ease-out`) for transform, background and border colour; press scales to 0.97.
- **Overlays:** fade in over 200ms and out over 140ms; dialogs scale from 0.96; menus grow from their trigger from 0.96 over 160ms and leave in 110ms.
- Reduced motion collapses every animation and transition.

### Homepage

#### Buttons
- **Shape:** a small accession label: square with a 10px top-left cut, at least 48px high, 22px right and 26px left padding, medium weight, optional 16px drawn arrow.
- **Fill:** blue-black with orange text in light theme; orange with blue-black text in dark theme.
- **Hover / Press:** lifts 2px on fine-pointer hover; scales to 0.97 on press; 160ms on the homepage ease.
- **Links:** underlined, 5px offset, 2px thick (1px in the header), 3px on hover. Sign in in the header is a link; in the footer it is the cut button at the small step.

#### Chips and Segments
- **Chip:** a radio label, 44px high, 8px cut, rest fill `currentColor` at 14%, chosen fill solid `currentColor` with the word in the field colour behind it (`--chip-on`). Focus is the 3px inset ring; a chosen chip under focus also underlines its word.
- **Depth segments:** the same chip blown up in em (0.5em cut) as the single large control in the depth section; choosing one repaints the whole section field (white for metadata, orange for sampled, blue-black for full) over 200ms.

#### Accession Stage and Slip (signature)
The hero stage is an orange label with a 72 to 128px cut that opens like a shutter from its cut corner. On it sits the live slip: a white label (44px cut) with the example title, the accession number at the head step, file and size, a barcode beside the mono SHA-256, then check rows divided by 2px rules at 12% ink. Each row ends in an outcome mark and its word ("Passed", "Finding", "Failed", "Clean", "Not reached", "Not tested"). A bordered release stamp sits bottom right at -2 degrees; when rejected it fills blue-black with orange text. Picking a sample reprints the rows and re-stamps.

#### Stage Bar
Five equal cells joined by 6px seams, each a flat tone stepping from the rest field (with a 2px inset ink outline) through tint 2, orange, rust, to blue-black. Each cell carries the stage name at the lead step and what happens inside at the small step; the clause sits below the cell.

#### Feature Board and Fragments
Tiles on a dense 12-column board, each a flat tone (tint 1, tint 2, white, or blue-black with orange). A `data-ui` close-up sits top-left, sized in em and cropped by the tile; name and line sit bottom-left. Tags inside fragments are square: outlined in `currentColor` at 0.12em, or solid blue-black with orange text when emphasised. Example values are labelled as examples.

#### Readiness Spines
Desktop: one open label (560px) beside folded 76px spines, all with a 16px cut, cycling tints 1 to 3. A spine is a button with the dimension name and status set vertically, the outcome mark between. Opening animates `flex-basis` over 300ms; reduced motion snaps. Below `lg` every dimension is a stacked card.

#### Connection Label
A white label (40px cut) on the orange field listing each storage provider at the lead step with what a connection needs, divided by 2px rules at 14% `currentColor`. No logos.

#### Start Band and Footer Label
The Start band is the whole link: blue-black with an orange poster word bottom-aligned, a 72 to 160px cut, a sentence and a drawn arrow top right; hover and focus swap to orange with blue-black ink over 150ms, focus outline inset 6px. The footer opens with the name at the display step on an orange label (56 to 120px cut) with the footer line and its barcode, then link columns at the small step.

#### Workflow map, fix chapters and feature index
- **Workflow map:** nine stage boxes on a 1008-unit drawing that never renders below 1:1, so its 13px labels stay 13px. Arrive, Quarantine gate, Catalog, Snapshot, Readiness and Use sit on one wired row; Second copy, Governance and Evidence are rails the full width of the path. A stage chooser (cut-corner chips) fills the chosen box, and a tint-1 chamfered label beside the drawing (the map sits on a white field) says what that stage fixes and which features belong to it, with a link to its chapter. The token rests on the chosen box's top edge, walks the wire row to a new choice, and walks the whole path once when the map first scrolls into view. Below `md` the drawing is dropped and the label sits directly under the chips.
- **Fix chapters:** one section per problem, alternating orange and white fields. The problem is the heading (head step, left), the fix is the paragraph (right), then the working flow diagram, then "Names and values in the diagram are examples." The problem always reads before the fix.
- **Feature index:** a real table (feature, problem, fix) inside a tint-1 chamfered label on orange, grouped under the map's nine stage names, with stage filter chips and an "N of 29 features" count.
- **Diagram variables (set per field inside `.home`):** `--fg` ink, `--box` box fill, `--mask` label ground. On orange fields `--box-end` and `--token` are blue-black and `--box-end-ink` orange, so a path's end and the token stand off the orange; on white they fall back to `--accent` orange. Boxes are solid when the chosen path reaches them, dashed when it does not, filled where it ends. `.home .choice` restyles the diagrams' chips as cut-corner labels (14% ink tint, filled with the field ink when chosen, inset 3px focus ring); switches keep the tint and show a tick.
- **One stage vocabulary:** the stage bar (Upload, Verify, Inspect, Promote, Pin) is the first four map stages up close, and the map's intro names that section to say so; the index groups use the map's names.

#### Navigation
Wordmark at the lead step on the left; four in-page links (small, medium, 1px underline) shown from `lg`; the console's ghost theme toggle at 44px; Sign in as an underlined link.

#### Motion
GSAP, all inside `gsap.matchMedia("(prefers-reduced-motion: no-preference)")`; the page reads complete without script.
- **Hero rise:** headline, sentence and button rise 24px and fade in reading order (700ms, power3.out, 80ms stagger).
- **Shutter:** the stage reveals left to right by `clip-path` inset (1.1s, power3.inOut, 200ms delay) while its contents settle from 1.06 scale at the top-left corner. This is the one loud moment.
- **Section reveals:** marked parts rise 24px and fade once at 92% of the viewport (600ms, power3.out, 70ms stagger, batched).
- **Slip reprint:** on a pointer choice, rows print in (320ms, 40ms stagger) and the stamp lands from 1.12 scale (300ms). Arrow-key choices and reduced motion change instantly.
- **UI transitions:** 150 to 160ms on `cubic-bezier(0.23, 1, 0.32, 1)` for transform, background and colour; press scales to 0.97.
- Smooth scroll is the app-wide Lenis instance from the root layout, shared with the console.

## Do's and Don'ts

### Do:
- **Do** treat the console and the homepage as separate worlds: console work uses the console tokens, homepage work uses the `home-*` tokens inside `.home`.
- **Do** keep the console's three levels: the frame holds the sidebar and top bar, work sits on the sheet, strips and panels sit on the sheet, and nothing nests deeper.
- **Do** separate console surfaces with tone and the 1px hairline, and put a well one tone step toward the frame.
- **Do** keep orange in the console to fills with ink on top, on what needs the user only; ink is the primary action.
- **Do** give every strip a filled 40px state cell with a drawn mark, and keep the filled square for a real pass: never-tested items stay hollow.
- **Do** use `--edge` on every console form control boundary.
- **Do** show status with a shape and a word together (OutcomeMark, StateBadge, Strip) on both surfaces.
- **Do** show "Not checked" as plainly as "Passed".
- **Do** keep console corners at 2px or 4px, with 12px for the sheet alone.
- **Do** make every homepage button, chip, segment and label an accession label: square, with only the top-left corner cut, sized to the element.
- **Do** let homepage colour own whole full-width fields and change only at hard horizontal seams; join adjacent cells with the 6px seam.
- **Do** keep homepage type to the five `h-*` steps plus the Start poster word, at medium weight.
- **Do** print the text a barcode stands for beside it, and keep the barcode `aria-hidden`.
- **Do** use mono only for IDs, keys, hashes, checksums and event names.
- **Do** respect reduced motion for GSAP (via `gsap.matchMedia`) and Lenis on both surfaces.

### Don't:
- **Don't** use gradients, glass or blur, glowing orbs, dot grids or neon on either surface.
- **Don't** use any box-shadow in the console, and don't put any shadow on the homepage except the inset focus ring.
- **Don't** wrap overview racks in cards: the strips are the cards.
- **Don't** set orange as text or a thin line on the light console sheet, or use it for navigation chrome or the primary button.
- **Don't** mark a strip with a coloured side border instead of the filled state cell.
- **Don't** bring the frame, the sheet, strips or hairline panels onto the homepage, or orange fields and cut corners into the console.
- **Don't** round a homepage corner or cut any corner other than the top-left.
- **Don't** use side stripes or rows of identical feature cards on the homepage.
- **Don't** use em dashes or emoji anywhere in copy or UI.
- **Don't** use Lucide, or Inter, Geist or Space Grotesk. Phosphor (console icons), drawn SVG marks (homepage) and Hanken Grotesk are the set.
- **Don't** tell status by hue alone.
- **Don't** add a privacy policy page, fake testimonials, logos, invented customer numbers or any unbacked claim.
