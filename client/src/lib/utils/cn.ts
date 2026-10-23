import clsx, { type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// twMerge lets a caller's class override the component's default (px-0 over px-4).
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
