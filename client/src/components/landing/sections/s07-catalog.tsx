import { copy } from "../copy";
import { Section } from "../section";
import { CatalogBoard } from "./catalog-board";

export function Catalog() {
  return (
    <Section stop="catalog" tone="plum" labelledBy="catalog-title">
      <div className="grid gap-6 lg:grid-cols-3" data-reveal="left">
        <div>
          <h2 id="catalog-title" className="t-h2">
            {copy.catalog.title}
          </h2>
          <p className="t-body mt-5 max-w-[46ch]">{copy.catalog.text}</p>
        </div>
      </div>
      <div className="mt-12 lg:mt-16" data-reveal="fade">
        <CatalogBoard />
      </div>
    </Section>
  );
}
