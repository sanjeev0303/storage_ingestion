import { Barcode } from "./motif";

// The one full-width band that splits the page in two: the label's barcode,
// enlarged. Decorative.
export function BarcodeBand() {
  return (
    <div aria-hidden className="field-night overflow-hidden py-6">
      <Barcode value="sha256:9f2c7a0e4b1d88c2f3a6e5d90b7c41e2a8f6d3b5c9e1a0f4d7b2c8e6a3f9d1e41b" height={64} className="h-16 w-full" />
    </div>
  );
}
