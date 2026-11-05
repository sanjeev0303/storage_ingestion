// The accession label's barcode, drawn from a string so the same text always
// gives the same bars. Decorative: the text it encodes is printed beside it.
export function Barcode({ value, height = 40, className }: { value: string; height?: number; className?: string }) {
  const bars: { x: number; w: number }[] = [];
  let x = 0;
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    const w = 1 + (code % 3);
    bars.push({ x, w });
    x += w + 1 + ((code >> 2) % 2);
  }
  return (
    <svg viewBox={`0 0 ${x} ${height}`} preserveAspectRatio="none" className={className} aria-hidden>
      {bars.map((b, i) => (
        <rect key={i} x={b.x} y={0} width={b.w} height={height} fill="currentColor" />
      ))}
    </svg>
  );
}
