---
name: Orbidune
description: Verified dataset storage for ML teams, presented as a dataset paper. One world for the homepage, the console and the auth pages.
colors:
  sheet: "#FFFFFF"
  frame: "#F0F1F7"
  orbit-ink: "#0E1130"
  ultramarine: "#2F3BFF"
  on-ultramarine: "#FFFFFF"
  ultramarine-soft: "#ECEDFF"
  saffron: "#FFC21A"
  on-saffron: "#0E1130"
  muted: "#4D4F66"
  faint: "#F1F1F3"
  well: "#F6F7FA"
  line: "#E2E2E6"
  control-line: "#CFCFD6"
  edge: "#7A7C8D"
  field-ultra-muted: "#E2E4FF"
  field-ultra-line: "#6972FF"
  field-night-muted: "#CFCFD6"
  field-night-line: "#393C55"
  dark-frame: "#070919"
  dark-sheet: "#0E112B"
  dark-ink: "#E8EAF8"
  dark-ultramarine: "#3D48FF"
  dark-ultramarine-text: "#A3A9FF"
  dark-ultramarine-soft: "#12163E"
  dark-muted: "#B8BACB"
  dark-faint: "#22253D"
  dark-well: "#0A0D21"
  dark-line: "#2F324A"
  dark-control-line: "#3A3C54"
  dark-edge: "#86889C"
  dark-field-ultra: "#2A33D6"
typography:
  display:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(2.125rem, 1rem + 2.9vw, 3.25rem)"
    fontWeight: 700
    lineHeight: 1.02
    letterSpacing: "-0.035em"
    fontVariation: "\"wdth\" 125"
  headline:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.875rem, 1.1rem + 2.4vw, 3.25rem)"
    fontWeight: 700
    lineHeight: 1.06
    letterSpacing: "-0.03em"
    fontVariation: "\"wdth\" 125"
  page-title:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.875rem"
    fontWeight: 700
    lineHeight: 1.25
    letterSpacing: "-0.025em"
    fontVariation: "\"wdth\" 125"
  lead:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.125rem, 1rem + 0.5vw, 1.375rem)"
    fontWeight: 400
    lineHeight: 1.4
    letterSpacing: "-0.01em"
  panel-title:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.125rem"
    fontWeight: 600
    lineHeight: 1.5
  figure-title:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 600
    lineHeight: 1.3
  body-paper:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.0625rem"
    fontWeight: 400
    lineHeight: 1.6
  body:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "Archivo, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 600
    lineHeight: 1.45
  data:
    fontFamily: "JetBrains Mono, ui-monospace, monospace"
    fontSize: "max(0.8125em, 13px)"
    fontWeight: 400
    lineHeight: 1.5
rounded:
  xs: "3px"
  sm: "4px"
  md: "6px"
  lg: "8px"
  figure: "10px"
  sheet: "14px"
  pill: "999px"
spacing:
  wrap-mobile: "20px"
  wrap-tablet: "32px"
  wrap-desktop: "48px"
  panel: "20px"
  section: "96px"
  section-wide: "128px"
  content-max: "80rem"
  console-max: "1400px"
components:
  button-orb:
    backgroundColor: "{colors.saffron}"
    textColor: "{colors.on-saffron}"
    rounded: "{rounded.pill}"
    padding: "0 22px"
    height: "48px"
  button-primary:
    backgroundColor: "{colors.ultramarine}"
    textColor: "{colors.on-ultramarine}"
    typography: "{typography.body}"
    rounded: "{rounded.md}"
    padding: "0 16px"
    height: "36px"
  button-secondary:
    backgroundColor: "{colors.sheet}"
    textColor: "{colors.orbit-ink}"
    typography: "{typography.body}"
    rounded: "{rounded.md}"
    padding: "0 16px"
    height: "36px"
  button-accent:
    backgroundColor: "{colors.saffron}"
    textColor: "{colors.on-saffron}"
    typography: "{typography.body}"
    rounded: "{rounded.md}"
    padding: "0 16px"
    height: "36px"
  input:
    backgroundColor: "{colors.well}"
    textColor: "{colors.orbit-ink}"
    typography: "{typography.body}"
    rounded: "{rounded.md}"
    padding: "0 12px"
    height: "36px"
  panel:
    backgroundColor: "{colors.sheet}"
    textColor: "{colors.orbit-ink}"
    rounded: "{rounded.md}"
    padding: "{spacing.panel}"
  figure:
    backgroundColor: "{colors.sheet}"
    textColor: "{colors.orbit-ink}"
    rounded: "{rounded.figure}"
  state-badge:
    backgroundColor: "{colors.faint}"
    textColor: "{colors.orbit-ink}"
    typography: "{typography.label}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  state-badge-failed:
    backgroundColor: "{colors.saffron}"
    textColor: "{colors.on-saffron}"
    typography: "{typography.label}"
    rounded: "{rounded.sm}"
    padding: "2px 8px"
  field-ultra:
    backgroundColor: "{colors.ultramarine}"
    textColor: "{colors.on-ultramarine}"
  field-night:
    backgroundColor: "{colors.orbit-ink}"
    textColor: "{colors.on-ultramarine}"
---

# Design System: Orbidune

## Overview

**Creative North Star: "The Dataset Paper"**

Orbidune is presented the way its readers already read evidence: as an ML research paper and a datasheet for a dataset. White paper, deep orbit ink, ultramarine as the figure colour, saffron as the one attention colour. Claims arrive as numbered figures in hairline frames, results as rule-only booktabs tables where "Not checked" is a first-class row, and a method that can be followed station by station. The homepage reads it at full Persuade strength: one full-strength ultramarine field for the abstract and Figure 1, one reading axis down the page, a night field for the close. The signed-in console and the auth pages render the same world in Operate register through the same tokens and primitives: a cool frame tint holding the sidebar and top bar, work on an inset white sheet, flat hairline panels, ultramarine for the one primary action.

Density is calm on the homepage (96 to 128px between sections, 17px paper body) and working-dense in the console (14px body, 36px controls, 20px panel padding). Everything except ultramarine and saffron is mixed from ink and the sheet, so the palette stays two colours plus paper in both themes. The world refuses the dark developer-platform hero with a neon accent and a feature-card grid, and the broadsheet-editorial costume (italic serif display, tracked mono labels).

Brand mark ("Orbit O", chosen 2026-11-12 over two other concepts kept in `client/public/brand/`): an orbit ring drawn as the O of the name, two dunes inside it, and the saffron orb riding the ring as its satellite, drawn in code (`components/brand/logo.tsx`). The ring and the lower dune take the text colour; the upper dune reads `--logo-dune` (ultramarine text by default, white at 45% on the ultramarine field, #7C84FF on night) and the gap round the orb reads `--logo-gap` (the ground it sits on: sheet, frame, ultramarine field or night). The app icon (`src/app/icon.svg`) sets the mark on an orbit-ink tile with a white ring.

**Key Characteristics:**
- White sheet, orbit ink text, ultramarine figures, saffron fills with ink on top.
- Archivo in two widths: expanded (wdth 125) for display, section heads, page titles, figure numbers and the wordmark; normal width for text.
- JetBrains Mono only for hashes, paths, object keys and code.
- Numbered figures in hairline frames with the number and title as a heading above the drawing.
- Booktabs tables: three rules, no verticals, tabular figures.
- Flat surfaces; depth by tone (frame, sheet, well) and hairlines.
- Status told by shape and word, never by hue alone.

## Colors

Paper and ink with two committed colours: ultramarine carries figures, links and the primary action; saffron is the single attention fill.

### Primary
- **Orbit Ultramarine** (`ultramarine`): the figure colour. Figure numbers, the orbit line and its stops, focus rings, carets, the active nav marker, the console's primary button, and the one full-strength homepage field. As text it uses the ultramarine-text role, which equals ultramarine in light and lifts to `dark-ultramarine-text` in dark so it keeps contrast. White (`on-ultramarine`) is the only text pairing on its fill.
- **Ultramarine Wash** (`ultramarine-soft`): ultramarine at 9% on the sheet; the dune wash inside Figure 1. Not a panel surface.

### Secondary
- **Signal Saffron** (`saffron`): the one attention colour. The homepage pill button, the console accent button, text selection, focus rings on coloured fields, the failed state badge, the orb in the logo, and the centre or fill of warning and failed outcome marks. Always a fill with orbit ink on top (`on-saffron`); identical in both themes.

### Neutral
- **Sheet** (`sheet`): the page and every working surface: panels, figures, the console sheet, controls.
- **Cool Frame** (`frame`): the console frame that holds the sidebar and top bar around the inset sheet.
- **Orbit Ink** (`orbit-ink`): body text, headings, booktabs rules, the active tab underline, the night field, the code panel ground.
- **Muted Ink** (`muted`): secondary text, descriptions, inactive nav. Ink at 74% on the sheet; 4.5:1 or better on sheet, frame and faint.
- **Faint** (`faint`): ink at 6%; quiet badges, hover fills on ghost buttons, pressed secondary buttons.
- **Well** (`well`): one tone step from the sheet toward the frame; input fills and recessed panels.
- **Hairline** (`line`): ink at 12%; panel, figure and row borders, the console sheet edge, section dividers.
- **Control Line** (`control-line`): ink at 20%; the border of secondary buttons and other controls on the sheet.
- **Edge** (`edge`): ink at 55%; form control boundaries (3:1 against the sheet) and the scrollbar thumb.
- **On-field tones** (`field-ultra-muted`, `field-ultra-line`, `field-night-muted`, `field-night-line`): white mixed into the field colour for secondary text and dividers on the ultramarine and night fields.

### Dark theme
The `dark-*` tokens replace their light counterparts under `data-theme="dark"` or the system dark preference (unless light is pinned). The sheet becomes a deep orbit blue (`dark-sheet`) inside a near-black frame (`dark-frame`); ink becomes a pale periwinkle (`dark-ink`). Ultramarine as a fill lifts slightly (`dark-ultramarine`) and as text lifts to `dark-ultramarine-text`. The ultramarine field darkens to `dark-field-ultra`; the night field drops to `dark-frame`. Saffron and on-saffron do not change.

### Named Rules
**The Saffron Is A Fill Rule.** Saffron is never text on the sheet. It is a fill, and whatever sits on it is orbit ink.

**The Two Colours Rule.** Ultramarine and saffron are the only hues. Every other tone is mixed from ink and the sheet; do not add a third hue for status, success or charts.

**The One Field Rule.** A surface gets at most one full-strength ultramarine field (the homepage hero, the auth aside). The night field is reserved for the close and footer.

## Typography

**Display Font:** Archivo, expanded on its width axis (wdth 125), with ui-sans-serif, system-ui fallback
**Body Font:** Archivo at normal width
**Label/Mono Font:** JetBrains Mono, data only

**Character:** One grotesque in two widths. Expanded bold Archivo gives headings the stance of a paper's title and figure labels; normal-width Archivo keeps reading text plain. Mono appears only where the content is literally machine text.

### Hierarchy
- **Display** (700, `clamp(2.125rem, 1rem + 2.9vw, 3.25rem)`, 1.02, -0.035em, expanded, balanced): the homepage h1 on the ultramarine field.
- **Headline** (700, `clamp(1.875rem, 1.1rem + 2.4vw, 3.25rem)`, 1.06, -0.03em, expanded, balanced): homepage section heads, the close, the auth aside claim.
- **Page title** (700, 1.875rem, 1.25, -0.025em, expanded): the console page h1.
- **Lead** (400, `clamp(1.125rem, 1rem + 0.5vw, 1.375rem)`, 1.4): the line under the display and close headings, about 34 to 38rem wide.
- **Panel title** (600, 1.125rem): console panel and section headings, normal width.
- **Figure title** (600, 0.9375rem, 1.3): figure and table titles, preceded by the number in expanded 700 ultramarine.
- **Body paper** (400, 1.0625rem, 1.6): homepage prose, about 30 to 36rem wide.
- **Body** (400, 0.875rem, 1.5): the console base size and controls.
- **Label** (600, 0.8125rem): state badges, nav group names, small counts. Sentence case, no tracking.
- **Data** (JetBrains Mono, `max(0.8125em, 13px)`): hashes, paths, keys, code; wraps anywhere.

### Named Rules
**The Two Widths Rule.** Expanded width is for display, headlines, page titles, figure numbers and the wordmark. Body, labels, panel titles and buttons stay at normal width.

**The Thirteen Pixel Floor Rule.** Nothing renders under 13px.

**The Mono Is Data Rule.** JetBrains Mono marks machine text only, never labels, kickers or decoration.

## Layout

The homepage runs on one content width (`content-max`) with side padding of 20px, 32px from 640px and 48px from 1024px, and a 12-column grid from 1024px. The hero fills the viewport: text on columns 1 to 6, Figure 1 on columns 7 to 12; it stacks below 1024px. Sections sit 96px apart, 128px from 1024px, divided where needed by a hairline. Section heads take columns 1 to 4 or 1 to 6 with their intro beside or below; tables and figures take the remaining columns or the full width.

The orbit line is a 2px ultramarine rail in the left margin from 1024px, running from the end of the hero down into the close, with a 14px ringed stop beside every section heading (filled saffron on the night field). It is hidden below 1024px. The direction first asked for one line threading every figure. On 2026-11-12 the owner chose the margin rail instead: a line drawn through Figures 2 and 3 would cross their drawings and labels. The rail is the rule; do not route it through figures. Figures 1 and 3 keep their own arcs.

The console is a 15rem sidebar plus a content column on the cool frame. The 48px top bar sits on the frame; the work sits on a white sheet with a hairline border and 14px radius, inset 8px from the window's right and bottom edges. Content runs to 1400px with 16, 24 and 32px side padding. Below 1024px the sheet is the screen and navigation moves into a modal. Touch targets are at least 44px on nav, links and header actions.

Auth pages split 5:7 on wide screens: the ultramarine field with logo and claim on the left, the form on the sheet on the right (max 48rem). Below 1024px only the sheet column shows. The auth field's bottom row carries the orbit-and-dunes scene (`components/brand/orbit-dunes.tsx`: two orbits round the risen saffron orb, a white satellite, three translucent white dune layers). The close field carries the dune horizon (`components/brand/dune-horizon.tsx`) as a full-bleed band between the close and the footer links: the orb half-risen behind the far ridge, its orbit overhead, flat night-blue dune layers ending in the night field itself. Both are hand-drawn flat vector art (the owner asked for them crafted in code rather than generated), and both move slowly: the satellite rides its orbit (GSAP motion path, `components/brand/orbit-satellite.tsx`, 22 to 30 s a loop, paused off screen; in the close it sets behind the dunes), the orb breathes (the close uses thin saffron rings, because a saffron wash over navy turns brown), the dashed outer orbit's dashes travel, the dune layers sway at different speeds, wind lines drift and the close's stars twinkle (`components/brand/art.css`, loops of 4.5 to 24 s). Under reduced motion both scenes rest. The 1200x630 share image (`src/app/opengraph-image.png`, copied to `twitter-image.png`) repeats the ultramarine field, the mark, the headline and the orbit-and-dunes scene.

## Elevation & Depth

Flat by default. Depth comes from tone (frame behind sheet, well inside sheet) and 1px hairlines, not shadows. The one shadow in the build is a soft ambient lift under Figure 1 where its white frame sits on the ultramarine field; it is a single instance, not a token for other surfaces. Overlays and menus separate by a scrim and their own border, entering with a short ease-out (200ms in, 140ms out; menus 160ms and 110ms, growing from the trigger).

### Named Rules
**The Tone Not Shadow Rule.** A surface is raised by sitting on a darker tone (sheet on frame) or recessed by a well; it is not lifted with a shadow.

## Shapes

Gently rounded rectangles: 6px on panels, buttons, inputs and wells; 4px on state badges; 8px on nav items; 10px on figure frames; 14px on the console sheet. Pills are reserved for the homepage's saffron button, the homepage header links and the orbit stops. Status marks are square: filled, half-filled, ringed, crossed or dashed squares at 12 to 16px. Drawings share one ellipse geometry: Figures 1 and 3 place their stations on an orbit arc, and the logo crosses its orb with a tilted orbit ellipse over a dune curve.

## Components

### Buttons
- **Shape:** gently rounded (6px) in the console; full pill for the homepage button.
- **Saffron pill (homepage):** saffron with orbit ink, 48px tall (44px in the header), 22px side padding, 600 weight at 1rem. The only filled button on the homepage. Hover (fine pointers only) lifts 2px and lightens 12% toward white; press scales to 0.97; 160ms ease-out.
- **Primary (console):** ultramarine with white, 36px tall, 16px padding, 500 weight. Hover darkens 12% toward black; press scales to 0.97. One per view.
- **Secondary:** sheet with a 1px control line; hover takes the edge colour, press fills faint.
- **Accent:** saffron with ink for a rare attention action in the console.
- **Ghost:** no fill; hover and press fill faint.
- **Text link (homepage):** 600 weight, 2px underline offset 6px, thickening to 3px on hover; at least 44px tall.

### Chips / State badges
- **Style:** 4px radius, 2px 8px padding, label type, a 12px square mark before the word.
- **State:** failed is a saffron fill with a crossed square; warning is faint with a ringed square holding a saffron centre; active is an ink fill with a play triangle; done is faint with a filled square; neutral is faint with no mark. Check outcomes use 16px marks: filled (passed), half-filled (partial), ringed with saffron centre (warning), saffron with a cross (failed), dashed (not checked).

### Cards / Containers
- **Corner Style:** 6px for panels, 10px for figures, 14px for the console sheet.
- **Background:** sheet for raised panels, well for recessed panels.
- **Shadow Strategy:** none (see Elevation & Depth).
- **Border:** 1px hairline.
- **Internal Padding:** 20px; the panel header has 20px top and sides, then 16px to content.

### Inputs / Fields
- **Style:** well fill, 1px edge border (3:1), 6px radius, 36px tall, 12px padding, muted placeholder, ultramarine caret. Native selects use a chevron drawn in the theme's ink.
- **Focus:** 2px ultramarine-text outline at 2px offset (the global focus ring).
- **Error / Disabled:** invalid adds a 2px ink outline plus a message; disabled drops to 50% opacity. Fields with a max length show a tabular "42 / 200" counter that turns semibold near the limit.

### Navigation
- **Console sidebar:** grouped list with group names in muted 600 label type; items 44px tall, 8px radius, 500 weight, Phosphor icon at 20px. Current item: sheet fill with hairline border, ink text, filled icon in ultramarine, a 4px ultramarine marker at the left edge. Others are muted and turn ink on hover.
- **Tabs:** route tabs on a hairline row; the current tab is a 2px ink underline. Long rows scroll sideways.
- **Homepage header:** logo left, four section links as pills in on-field muted turning white on hover, Sign in, then the saffron pill. On phones the header pill hides and the hero button is the one action.

### Figure
The signature container. A sheet with a 1px hairline frame and 10px radius. Its head is a row (14px 18px 12px padding, hairline below) holding the heading: the figure or table number in expanded 700 ultramarine, then the title at 600 0.9375rem. Optional controls (Replay figure, mode toggles) sit right. The drawing takes Archivo for labels and JetBrains Mono for file names and hashes, and draws in theme colours so it works on the sheet and on a field. Figures animate with reduced-motion fallbacks: Figure 1 moves a file from upload through verify, inspect, scan and catalog to a sealed snapshot along an orbit arc, the active station ringing once a beat; Figure 3 draws the method arc as the section scrolls on wide screens.

**Problem figures (Figures 2 and 3).** Problem and fix are shown side by side or in sequence, never behind a switch. Figure 2 sets each failure as a row: a neutral "Bucket and spreadsheet" panel, an ultramarine arrow, then a tinted "With Orbidune" panel, each drawing looping its story in 4 s. Figure 3 is four cards, each an 8 s loop that plays the problem, then the same moment with Orbidune, labelled in the drawing. Both are `LiveFigure` (`components/home/live-figure.tsx`): loops run only while on screen, a Pause animation control sits in the head, and the CSS in `components/home/figures.css` moves only transform and opacity. A part's panel visibility and its animation never share an element, since both set opacity. Under reduced motion, or before JavaScript runs, every drawing rests in its fixed state, and the two sentences above each drawing tell the whole story without motion.

### Booktabs table
A table title in the figure-title style with its number, then three rules and no verticals: 2px ink above the header, 1px ink under it, hairlines between rows, 2px ink closing the body. Cells have 12px padding, tabular figures, row headers in 600. Outcomes are a square mark plus a word.

### Fields (ultramarine and night)
Full-bleed coloured grounds with white text. The ultramarine field holds the homepage hero and the auth aside; the night field holds the close and footer. Each carries its own muted and line tones and switches focus rings to saffron. Figures set on a field keep their white sheet.

### Code panel
In the integrations figure, code renders in JetBrains Mono 0.875rem on an orbit-ink ground with pale ink text in both themes, under a framework tab row.

## Do's and Don'ts

### Do:
- **Do** put the figure or table number above the drawing as part of its heading, in expanded ultramarine, followed by the title.
- **Do** use booktabs rules (2px ink, 1px ink, hairlines, 2px ink) for every results or comparison table, with tabular figures.
- **Do** keep "Not checked" visible as its own outcome, drawn as a dashed square.
- **Do** tell status apart by shape and word: filled, half, ringed, crossed and dashed squares.
- **Do** put orbit ink on saffron and white on ultramarine.
- **Do** mix new neutral tones from ink and the sheet instead of introducing a grey.
- **Do** keep one primary action per view: the saffron pill on the homepage, the ultramarine button in the console.
- **Do** give every animation a reduced-motion path that shows the static, complete state.

### Don't:
- **Don't** set saffron as text or as a thin line on the sheet.
- **Don't** caption figures below the drawing; the number and title head the figure.
- **Don't** use JetBrains Mono for labels, kickers or decorative tags; it is for hashes, paths and code.
- **Don't** add vertical rules or zebra fills to booktabs tables.
- **Don't** lift panels with shadows; use the frame, sheet and well tones.
- **Don't** build a dark developer-platform hero with a neon accent, or a grid of feature cards; features are a numbered table.
- **Don't** use an italic serif display or tracked uppercase labels.
- **Don't** add a third hue for success, warning or charts.
