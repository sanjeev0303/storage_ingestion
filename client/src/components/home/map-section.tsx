import { map } from "./home-copy";
import { PlatformMap } from "./platform-map";

export function MapSection() {
  return (
    <section id="map" aria-labelledby="map-title" className="field-b py-24 lg:py-32">
      <div className="mx-auto max-w-[1440px] px-4 sm:px-10">
        <div className="grid gap-6 lg:grid-cols-12">
          <h2 id="map-title" data-reveal className="h-head lg:col-span-6">
            {map.heading}
          </h2>
          <p data-reveal className="h-body max-w-[46ch] lg:col-span-5 lg:col-start-8 lg:self-end">
            {map.intro}
          </p>
        </div>
        <div data-reveal className="mt-12 lg:mt-16">
          <PlatformMap />
        </div>
      </div>
    </section>
  );
}
