"use client";

import { Check, Circle, Eye, EyeSlash } from "@phosphor-icons/react";
import { forwardRef, useState, type InputHTMLAttributes } from "react";
import { cn } from "@/lib/utils/cn";
import type { PasswordRule } from "@/lib/validation/password";
import { Input } from "./field";

// A password input with a button to show what was typed. It stays visible until
// toggled back, because people check a long password and then keep typing.
export const PasswordInput = forwardRef<HTMLInputElement, Omit<InputHTMLAttributes<HTMLInputElement>, "type">>(
  function PasswordInput({ className, ...rest }, ref) {
    const [visible, setVisible] = useState(false);
    return (
      <div className="relative">
        <Input ref={ref} type={visible ? "text" : "password"} className={cn("pr-11", className)} {...rest} />
        <button
          type="button"
          onClick={() => setVisible(!visible)}
          aria-pressed={visible}
          aria-label={visible ? "Hide password" : "Show password"}
          className="absolute top-0 right-0 inline-flex size-9 items-center justify-center text-muted hover:text-ink"
        >
          {visible ? <EyeSlash aria-hidden /> : <Eye aria-hidden />}
        </button>
      </div>
    );
  },
);

// The requirements, checked off as the person types. Each rule says met or not
// in words as well as by icon.
export function PasswordRules({ rules }: { rules: PasswordRule[] }) {
  return (
    <ul aria-label="Requirements for your password" className="flex flex-col gap-1 text-sm">
      {rules.map((rule) => (
        <li key={rule.label} className={cn("flex items-center gap-1.5", rule.met ? "font-medium" : "text-muted")}>
          {rule.met ? <Check aria-hidden weight="bold" /> : <Circle aria-hidden />}
          {rule.label}
          <span className="sr-only">{rule.met ? ", met" : ", not met yet"}</span>
        </li>
      ))}
    </ul>
  );
}
