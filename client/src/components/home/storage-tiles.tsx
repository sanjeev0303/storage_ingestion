import { storage } from "./home-copy";

// Where the bytes can live, as one connection label: each row says what a
// connection to that provider needs. No logos are drawn.
export function StorageTiles() {
  return (
    <section id="storage" aria-labelledby="storage-title" className="field-a py-24 lg:py-32">
      <div className="mx-auto grid max-w-[1440px] gap-10 px-4 sm:px-10 lg:grid-cols-12">
        <div data-reveal className="lg:col-span-4">
          <h2 id="storage-title" className="h-head">
            {storage.heading}
          </h2>
          <p className="h-body mt-6 max-w-[34ch]">{storage.sentence}</p>
          <p className="h-body mt-6 max-w-[34ch]">{storage.lifecycle}</p>
        </div>

        <div data-reveal data-ui className="label-cut bg-[var(--h-b)] p-6 text-[var(--h-ink)] [--cut:40px] sm:p-10 lg:col-span-8">
          <p className="h-small font-medium">{storage.labelTitle}</p>
          <dl className="mt-4 divide-y-2 divide-[color-mix(in_srgb,currentColor_14%,transparent)]">
            {storage.providers.map((p) => (
              <div key={p.name} className="grid gap-1 py-4 sm:grid-cols-[minmax(0,1fr)_minmax(0,1.3fr)] sm:gap-6">
                <dt className="h-lead">{p.name}</dt>
                <dd className="h-body sm:self-center">{p.needs}</dd>
              </div>
            ))}
          </dl>
        </div>
      </div>
    </section>
  );
}
