"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { ScrollTrigger } from "gsap/ScrollTrigger";
import { useRouter } from "next/navigation";
import { useId, useRef, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { BUCKET_MAX, NAME_MAX, REGION_MAX, bucketError, regionError } from "@/features/storage-connections/schema";
import { cleanText } from "@/lib/utils/forgive";
import { copy } from "../copy";

gsap.registerPlugin(useGSAP, ScrollTrigger);

const providers = copy.connect.providers.slice(1);
const input = "neo-in-sm h-11 w-full rounded-sm bg-ground px-3 text-ink placeholder:text-muted";

export function BeginDialog() {
  const root = useRef<HTMLFormElement>(null);
  const id = useId();
  const [name, setName] = useState("");
  const [bucket, setBucket] = useState("");
  const [region, setRegion] = useState("us-east-1");
  const [touched, setTouched] = useState<string[]>([]);
  const router = useRouter();
  const errors = {
    name: cleanText(name) ? undefined : "Enter a name for this connection.",
    bucket: bucket.trim() ? bucketError(bucket) : "Enter the bucket name.",
    region: regionError(region),
  };
  const ready = !errors.name && !errors.bucket && !errors.region;
  const firstProblem = errors.name ?? errors.bucket ?? errors.region;

  useGSAP(
    () => {
      const mm = gsap.matchMedia();
      mm.add("(prefers-reduced-motion: no-preference)", () => {
        gsap.from(root.current, {
          y: 24,
          autoAlpha: 0,
          duration: 0.2,
          ease: "power2.out",
          scrollTrigger: { trigger: root.current, start: "top 90%", once: true },
        });
      });
    },
    { scope: root },
  );

  function submit(event: FormEvent) {
    event.preventDefault();
    if (ready) router.push("/login");
  }

  function touch(field: string) {
    if (!touched.includes(field)) setTouched([...touched, field]);
  }

  function shown(field: keyof typeof errors) {
    const message = touched.includes(field) ? errors[field] : undefined;
    if (!message) return null;
    return (
      <p id={`${id}-${field}-error`} role="alert" className="t-small font-medium">
        {message}
      </p>
    );
  }

  function describedBy(field: keyof typeof errors) {
    return touched.includes(field) && errors[field] ? `${id}-${field}-error` : undefined;
  }

  return (
    <form
      ref={root}
      data-ui
      aria-labelledby={`${id}-title`}
      onSubmit={submit}
      data-taste-skill-ignore className="flex w-full max-w-[440px] flex-col gap-5 rounded-md border-2 border-on-accent bg-ground p-6 text-ink"
    >
      <h3 id={`${id}-title`} className="t-lead font-semibold">
        {copy.begin.dialogTitle}
      </h3>

      <div className="flex flex-col gap-1.5">
        <label htmlFor={`${id}-name`} className="t-body font-medium">
          Name <span className="font-normal">(required)</span>
        </label>
        <input
          id={`${id}-name`}
          name="connection-name"
          className={input}
          placeholder="Production bucket"
          autoComplete="off"
          maxLength={NAME_MAX}
          aria-invalid={touched.includes("name") && !!errors.name}
          aria-describedby={describedBy("name")}
          value={name}
          onChange={(e) => setName(e.target.value)}
          onBlur={() => touch("name")}
        />
        <p className="t-small text-right tabular-nums">
          {name.length} / {NAME_MAX}
        </p>
        {shown("name")}
      </div>

      <div className="flex flex-col gap-1.5">
        <label htmlFor={`${id}-provider`} className="t-body font-medium">
          Provider
        </label>
        <select id={`${id}-provider`} name="provider" className={input} defaultValue={providers[0]}>
          {providers.map((provider) => (
            <option key={provider}>{provider}</option>
          ))}
        </select>
      </div>

      <div className="grid gap-5 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <label htmlFor={`${id}-bucket`} className="t-body font-medium">
            Bucket <span className="font-normal">(required)</span>
          </label>
          <input
            id={`${id}-bucket`}
            name="bucket"
            spellCheck={false}
            className={input}
            placeholder="my-training-data"
            autoComplete="off"
            maxLength={BUCKET_MAX}
            aria-invalid={touched.includes("bucket") && !!errors.bucket}
            aria-describedby={describedBy("bucket")}
            value={bucket}
            onChange={(e) => setBucket(e.target.value)}
            onBlur={() => touch("bucket")}
          />
          <p className="t-small text-right tabular-nums">
            {bucket.length} / {BUCKET_MAX}
          </p>
          {shown("bucket")}
        </div>
        <div className="flex flex-col gap-1.5">
          <label htmlFor={`${id}-region`} className="t-body font-medium">
            Region
          </label>
          <input
            id={`${id}-region`}
            name="region"
            spellCheck={false}
            className={input}
            placeholder="eu-west-1"
            autoComplete="off"
            maxLength={REGION_MAX}
            aria-invalid={touched.includes("region") && !!errors.region}
            aria-describedby={describedBy("region")}
            value={region}
            onChange={(e) => setRegion(e.target.value)}
            onBlur={() => touch("region")}
          />
          <p className="t-small text-right tabular-nums">
            {region.length} / {REGION_MAX}
          </p>
          {shown("region")}
        </div>
      </div>

      <p className="t-small">Credentials are sent once and never shown again. You will be asked to sign in first.</p>

      <div className="flex flex-col items-start gap-1.5">
        <Button type="submit" variant="primary" disabled={!ready} className="h-12 rounded-sm px-8">
          Create connection
        </Button>
        {!ready && <p className="t-small">{firstProblem}</p>}
      </div>
    </form>
  );
}
