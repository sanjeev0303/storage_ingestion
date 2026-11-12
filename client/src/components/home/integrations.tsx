"use client";

import { HardDrives } from "@phosphor-icons/react";
import { useState } from "react";
import { LogoMark } from "@/components/brand/logo";
import { cn } from "@/lib/utils/cn";
import { integrations } from "./copy";
import { Wrap } from "./wrap";

// Real marks for real services (public/icons, fetched by the taste icon resolver).
const STORAGE_ICON: Record<string, string> = {
  s3: "/icons/aws-s3.svg",
  r2: "/icons/cloudflare.svg",
  gcs: "/icons/google-cloud.svg",
  azure: "/icons/azure.svg",
};
const FRAMEWORK_ICON: Record<string, string> = {
  torch: "/icons/pytorch.svg",
  tf: "/icons/tensorflow.svg",
  hf: "/icons/hugging-face.svg",
  jax: "/icons/jax.svg",
};

function StorageIcon({ id }: { id: string }) {
  if (id === "platform") return <LogoMark className="size-6" />;
  if (id === "s3c") return <HardDrives aria-hidden className="size-6 text-ultra-text" weight="duotone" />;
  // eslint-disable-next-line @next/next/no-img-element
  return <img src={STORAGE_ICON[id]} alt="" width={24} height={24} className="size-6 object-contain" loading="lazy" />;
}

export function Integrations() {
  const [tab, setTab] = useState(integrations.frameworks[0].id);
  const current = integrations.frameworks.find((f) => f.id === tab) ?? integrations.frameworks[0];

  function onKey(e: React.KeyboardEvent, i: number) {
    const n = integrations.frameworks.length;
    const next = e.key === "ArrowRight" ? (i + 1) % n : e.key === "ArrowLeft" ? (i - 1 + n) % n : -1;
    if (next < 0) return;
    e.preventDefault();
    setTab(integrations.frameworks[next].id);
    document.getElementById(`fw-${integrations.frameworks[next].id}`)?.focus();
  }

  return (
    <section id="integrations" className="scroll-mt-20 border-t border-line py-24 lg:py-32" aria-labelledby="integrations-title">
      <Wrap>
        <div className="grid gap-x-10 gap-y-5 lg:grid-cols-12">
          <h2 id="integrations-title" className="t-h2 orbit-stop lg:col-span-7">
            {integrations.heading}
          </h2>
          <p className="t-body max-w-[34rem] text-muted lg:col-span-4 lg:col-start-9 lg:self-end">{integrations.intro}</p>
        </div>

        <div className="mt-12 grid gap-6 lg:grid-cols-12">
          <div className="figure min-w-0 lg:col-span-5">
            <div className="figure-head">
              <h3>
                <span className="fig-no">Table 3</span> {integrations.storageTitle}
              </h3>
            </div>
            <ul>
              {integrations.storage.map((s) => (
                <li key={s.id} className="flex min-h-16 items-center gap-4 border-b border-line px-[18px] last:border-b-0">
                  <span className="grid size-10 shrink-0 place-items-center rounded-lg bg-well ring-1 ring-line">
                    <StorageIcon id={s.id} />
                  </span>
                  <span className="min-w-0">
                    <span className="block font-semibold">{s.name}</span>
                    <span className="block text-sm text-muted">{s.needs}</span>
                  </span>
                </li>
              ))}
            </ul>
          </div>

          <div className="figure flex min-w-0 flex-col self-start lg:col-span-7">
            <div className="figure-head flex-wrap">
              <h3>
                <span className="fig-no">Figure 5</span> {integrations.codeTitle}
              </h3>
            </div>
            <div role="tablist" aria-label="Framework" className="flex flex-wrap gap-1.5 border-b border-line px-3.5 py-3">
              {integrations.frameworks.map((f, i) => {
                const selected = f.id === tab;
                return (
                  <button
                    key={f.id}
                    id={`fw-${f.id}`}
                    type="button"
                    role="tab"
                    aria-selected={selected}
                    aria-controls="fw-panel"
                    tabIndex={selected ? 0 : -1}
                    onClick={() => setTab(f.id)}
                    onKeyDown={(e) => onKey(e, i)}
                    className={cn(
                      "inline-flex min-h-11 items-center gap-2 rounded-full px-3.5 text-sm font-medium transition-[background-color,color,transform] duration-150 ease-out active:scale-[0.97]",
                      selected ? "bg-ink text-ground" : "text-muted hover:bg-faint hover:text-ink",
                    )}
                  >
                    {FRAMEWORK_ICON[f.id] && (
                      // eslint-disable-next-line @next/next/no-img-element
                      <img src={FRAMEWORK_ICON[f.id]} alt="" width={18} height={18} className="size-[18px] object-contain" loading="lazy" />
                    )}
                    {f.name}
                  </button>
                );
              })}
            </div>
            <div id="fw-panel" role="tabpanel" aria-labelledby={`fw-${current.id}`} className="min-w-0 bg-[#0e1130] px-5 py-6 text-[#e8eaf8]">
              <pre translate="no" className="overflow-x-auto font-mono text-[0.875rem] leading-relaxed">
                <code>{current.code}</code>
              </pre>
            </div>
            <p className="flex flex-wrap items-center gap-3 border-t border-line px-[18px] py-3.5 text-sm text-muted">
              <span className="flex items-center gap-1.5" aria-hidden>
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img src="/icons/python.svg" alt="" width={18} height={18} className="size-[18px]" loading="lazy" />
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img src="/icons/typescript.svg" alt="" width={18} height={18} className="size-[18px]" loading="lazy" />
              </span>
              {integrations.sdks}
            </p>
          </div>
        </div>
      </Wrap>
    </section>
  );
}
