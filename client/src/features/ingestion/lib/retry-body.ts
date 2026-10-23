export const MAX_RETRY_POSITIONS = 1000;

// The retry route needs a body that says which entries. An empty body is a 400.
export function buildRetryBody(text: string): { positions: number[] } | { all: true } | { error: string } {
  if (text.trim() === "") return { all: true };

  const positions: number[] = [];
  for (const piece of text.split(/[\s,]+/).filter(Boolean)) {
    const value = Number(piece);
    if (!Number.isInteger(value) || value < 1) return { error: `"${piece}" is not an entry number. Use whole numbers from 1.` };
    positions.push(value);
  }
  if (positions.length > MAX_RETRY_POSITIONS) return { error: `Retry at most ${MAX_RETRY_POSITIONS} entries at a time, or leave the box empty to retry all.` };
  return { positions: [...new Set(positions)] };
}
