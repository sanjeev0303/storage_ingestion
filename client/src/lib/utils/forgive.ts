// Accept what people actually type or paste, and clean it before it is checked.

export function cleanEmail(value: string) {
  return value.trim().toLowerCase();
}

// "1,000", "1 000" and " 42 " all mean a whole number. Returns "" when it is not one.
export function cleanWholeNumber(value: string) {
  const digits = value.replace(/[\s,_]/g, "");
  return /^\d+$/.test(digits) ? digits : "";
}

// Ids and labels pasted one per line, or separated by commas or spaces, with stray quotes.
export function splitList(value: string) {
  return value
    .split(/[\s,;]+/)
    .map((item) => item.replace(/^["'`]+|["'`]+$/g, ""))
    .filter(Boolean);
}

// Collapses runs of spaces and trims, for names typed with extra spacing.
export function cleanText(value: string) {
  return value.replace(/\s+/g, " ").trim();
}

// A message when the text is not shaped like an email address, otherwise "".
export function emailError(value: string) {
  const email = cleanEmail(value);
  if (email === "") return "Enter an email address.";
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email) ? "" : "Enter a valid email address, like name@example.com.";
}
