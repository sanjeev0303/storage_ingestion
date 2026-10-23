import { CircleNotch } from "@phosphor-icons/react/dist/ssr";
import Link from "next/link";
import { forwardRef, type ButtonHTMLAttributes, type ComponentProps } from "react";
import { cn } from "@/lib/utils/cn";

export type ButtonVariant = "primary" | "secondary" | "accent" | "ghost";

const base =
  "inline-flex h-9 shrink-0 items-center justify-center gap-2 rounded-md px-4 text-base font-medium whitespace-nowrap " +
  "transition-[box-shadow,transform,background-color] duration-150 ease-out " +
  "disabled:pointer-events-none disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50";

const variants: Record<ButtonVariant, string> = {
  primary: "bg-ink text-ground shadow-neo-sm active:scale-[0.97] active:shadow-none",
  secondary: "neo-sm active:scale-[0.97] active:shadow-neo-in",
  accent: "bg-accent text-on-accent shadow-neo-sm active:scale-[0.97] active:shadow-none",
  ghost: "hover:bg-faint active:scale-[0.97] active:bg-faint",
};

export function buttonClass(variant: ButtonVariant = "secondary", className?: string) {
  return cn(base, variants[variant], className);
}

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  loading?: boolean;
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = "secondary", loading = false, className, children, disabled, type = "button", ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      className={buttonClass(variant, className)}
      {...rest}
    >
      {loading && <CircleNotch className="animate-spin" aria-hidden />}
      {children}
    </button>
  );
});

export function ButtonLink({
  variant = "secondary",
  className,
  ...rest
}: ComponentProps<typeof Link> & { variant?: ButtonVariant }) {
  return <Link className={buttonClass(variant, className)} {...rest} />;
}
