import type { ReactNode } from "react";
import { board } from "./home-copy";
import { Barcode } from "./motif";

type TileId = (typeof board.tiles)[number]["id"];

// Where each tile sits on the 12-column board, and its tone. The board has no
// empty cell: rows 1-2 heading and snapshots, 3-6 holds beside copies and
// lineage, 5-6 adopt, 7-8 streaming, evidence and the edge agent.
const PLACE: Record<TileId | "heading", string> = {
  heading: "lg:col-span-6 lg:row-span-2 col-span-2 row-span-2",
  snapshots: "lg:col-span-6 lg:row-span-4 col-span-2 row-span-3 bg-[var(--h-tint-1)] text-[var(--h-ink)]",
  holds: "lg:col-span-3 lg:row-span-4 col-span-1 row-span-2 bg-[var(--h-night)] text-[var(--h-orange)]",
  copies: "lg:col-span-3 lg:row-span-2 col-span-1 row-span-2 bg-[var(--h-b)] text-[var(--h-ink)]",
  lineage: "lg:col-span-3 lg:row-span-2 col-span-1 row-span-2 bg-[var(--h-tint-2)] text-[var(--h-ink)]",
  adopt: "lg:col-span-6 lg:row-span-2 col-span-2 row-span-3 bg-[var(--h-night)] text-[var(--h-orange)]",
  stream: "lg:col-span-6 lg:row-span-2 col-span-2 row-span-3 bg-[var(--h-b)] text-[var(--h-ink)]",
  audit: "lg:col-span-3 lg:row-span-2 col-span-1 row-span-2 bg-[var(--h-tint-1)] text-[var(--h-ink)]",
  edge: "lg:col-span-3 lg:row-span-2 col-span-2 row-span-2 bg-[var(--h-night)] text-[var(--h-orange)]",
};

// Each tile's close-up: a real piece of the console in em, enlarged and
// pushed off-centre so the tile's edges crop it.
const CLOSE_UP: Record<TileId, ReactNode> = {
  snapshots: (
    <div className="label-cut w-full max-w-[22em] bg-[var(--h-b)] p-[1.2em] text-[var(--h-ink)] [--cut:1.4em]">
      <div className="flex items-center justify-between">
        <span className="text-[1.6em] font-medium">Version 3</span>
        <span className="bg-[var(--h-night)] px-[0.6em] py-[0.2em] font-medium text-[var(--h-orange)]">Pinned</span>
      </div>
      <p className="mono-data mt-[0.6em]">manifest 4e1a…9b20</p>
      <div className="mt-[0.8em] flex gap-[0.4em]">
        {["m1", "m2", "m4"].map((m) => (
          <span key={m} className="mono-data border-[0.12em] border-current px-[0.5em] py-[0.15em]">
            {m}
          </span>
        ))}
      </div>
    </div>
  ),
  holds: (
    <div className="flex items-center gap-[0.6em]">
      <span className="relative inline-block h-[1.6em] w-[2.9em] bg-[var(--h-orange)]">
        <span className="absolute top-[0.2em] right-[0.2em] h-[1.2em] w-[1.2em] bg-[var(--h-night)]" />
      </span>
      <span className="text-[1.3em] font-medium whitespace-nowrap">Legal hold</span>
    </div>
  ),
  copies: (
    <div className="flex items-center gap-[0.5em]">
      <svg viewBox="0 0 16 16" className="h-[1.4em] w-[1.4em]" aria-hidden>
        <rect x="1.5" y="1.5" width="13" height="13" fill="currentColor" />
      </svg>
      <span className="text-[1.6em] font-medium">Verified</span>
    </div>
  ),
  lineage: (
    <div className="flex flex-wrap items-center gap-[0.5em]">
      <span className="border-[0.12em] border-current px-[0.5em] py-[0.3em] font-medium whitespace-nowrap">Snapshot 3</span>
      <span className="font-medium whitespace-nowrap">trained on</span>
      <span className="bg-[var(--h-night)] px-[0.5em] py-[0.3em] font-medium whitespace-nowrap text-[var(--h-orange)]">Model</span>
    </div>
  ),
  adopt: (
    <div className="flex flex-wrap gap-[0.4em]">
      {["S3 Inventory", "ASC MHL", "BagIt", "STAC"].map((s, i) => (
        <span key={s} className={`border-[0.12em] border-current px-[0.6em] py-[0.35em] font-medium whitespace-nowrap ${i === 1 ? "bg-[var(--h-orange)] text-[var(--h-night)]" : ""}`}>
          {s}
        </span>
      ))}
    </div>
  ),
  stream: (
    <div className="flex flex-wrap gap-[0.4em]">
      {["PyTorch", "TensorFlow", "JAX", "Ray"].map((s, i) => (
        <span key={s} className={`px-[0.7em] py-[0.5em] font-medium whitespace-nowrap ${i === 0 ? "bg-[var(--h-night)] text-[var(--h-orange)]" : "border-[0.12em] border-current"}`}>
          {s}
        </span>
      ))}
    </div>
  ),
  audit: (
    <div className="grid gap-[0.3em]">
      {["#2 asset.access", "#3 audit.export"].map((r) => (
        <span key={r} className="mono-data whitespace-nowrap">
          {r}
        </span>
      ))}
      <Barcode value="anchor-7a3f6e90bc" className="h-[1.4em] w-[9em]" />
    </div>
  ),
  edge: (
    <div className="grid gap-[0.2em]">
      <span className="text-[1.4em] font-medium">3 files waiting</span>
      <span>window 19:00 to 07:00</span>
    </div>
  ),
};

export function FeatureBoard() {
  return (
    <section id="platform" aria-labelledby="platform-title" className="field-a py-24 lg:py-32">
      <div className="mx-auto max-w-[1440px] px-[10px] sm:px-10">
        <ul className="grid grid-flow-row-dense auto-rows-[28vw] grid-cols-2 gap-[6px] lg:auto-rows-[104px] lg:grid-cols-12 lg:gap-2">
          <li className={`${PLACE.heading} flex items-start p-5`}>
            <h2 id="platform-title" data-reveal className="h-head max-w-[12ch]">
              {board.heading}
            </h2>
          </li>
          {board.tiles.map((tile) => (
            <li key={tile.id} data-reveal className={`relative overflow-hidden ${PLACE[tile.id]}`}>
              <div data-ui aria-hidden className="absolute top-5 right-5 left-5 text-[clamp(0.8125rem,1.15vw,1.125rem)]">
                {CLOSE_UP[tile.id]}
              </div>
              <div className="absolute right-5 bottom-5 left-5">
                <h3 className="h-body font-medium">{tile.name}</h3>
                <p className="h-small mt-1 max-w-[32ch]">{tile.line}</p>
              </div>
            </li>
          ))}
        </ul>
        <p className="h-small mt-4">Values in the close-ups are examples.</p>
      </div>
    </section>
  );
}
