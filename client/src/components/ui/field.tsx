"use client";

import { Warning } from "@phosphor-icons/react/dist/ssr";
import {
  forwardRef,
  useEffect,
  useId,
  useImperativeHandle,
  useRef,
  useState,
  type ForwardedRef,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { cn } from "@/lib/utils/cn";

const control =
  "neo-in-sm w-full border border-edge px-3 text-base text-ink placeholder:text-muted " +
  "disabled:opacity-50 aria-[invalid=true]:outline-2 aria-[invalid=true]:outline-offset-0 aria-[invalid=true]:outline-ink";

// "42 / 200". Shown whenever a control has a maxLength. It reads the element
// itself, so it also follows values a form sets from code.
function CharCount({ length, max }: { length: number; max: number }) {
  const near = length >= max * 0.9;
  return (
    <p className={cn("text-right text-sm tabular-nums", near ? "font-semibold" : "text-muted")}>
      {length} / {max}
    </p>
  );
}

function useLength<T extends HTMLInputElement | HTMLTextAreaElement>(forwarded: ForwardedRef<T>) {
  const ref = useRef<T>(null);
  const [length, setLength] = useState(0);
  useImperativeHandle(forwarded, () => ref.current as T);
  // Runs after every render and only sets state when the length really changed.
  // eslint-disable-next-line react-hooks/exhaustive-deps -- the element value is not a dependency React can see
  useEffect(() => {
    const now = ref.current?.value.length ?? 0;
    if (now !== length) setLength(now);
  });
  return { ref, length, setLength };
}

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(function Input(
  { className, maxLength, onInput, ...rest },
  forwarded,
) {
  const { ref, length, setLength } = useLength(forwarded);
  const box = (
    <input
      ref={ref}
      maxLength={maxLength}
      onInput={(event) => {
        setLength(event.currentTarget.value.length);
        onInput?.(event);
      }}
      className={cn(control, "h-9", className)}
      {...rest}
    />
  );
  if (!maxLength) return box;
  return (
    <div className="flex flex-col gap-1">
      {box}
      <CharCount length={length} max={maxLength} />
    </div>
  );
});

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(
  function Textarea({ className, maxLength, onInput, ...rest }, forwarded) {
    const { ref, length, setLength } = useLength(forwarded);
    const box = (
      <textarea
        ref={ref}
        maxLength={maxLength}
        onInput={(event) => {
          setLength(event.currentTarget.value.length);
          onInput?.(event);
        }}
        className={cn(control, "min-h-24 py-2", className)}
        {...rest}
      />
    );
    if (!maxLength) return box;
    return (
      <div className="flex flex-col gap-1">
        {box}
        <CharCount length={length} max={maxLength} />
      </div>
    );
  },
);

export const Select = forwardRef<HTMLSelectElement, SelectHTMLAttributes<HTMLSelectElement>>(function Select(
  { className, children, ...rest },
  ref,
) {
  return (
    <select ref={ref} className={cn(control, "h-9 appearance-auto", className)} {...rest}>
      {children}
    </select>
  );
});

type FieldProps = {
  label: string;
  hint?: ReactNode;
  error?: string;
  required?: boolean;
  className?: string;
  // Receives the ids to put on the control so the label, hint and error are announced with it.
  children: (props: { id: string; "aria-describedby"?: string; "aria-invalid"?: boolean }) => ReactNode;
};

export function Field({ label, hint, error: liveError, required, className, children }: FieldProps) {
  const id = useId();
  const wrapper = useRef<HTMLDivElement>(null);
  // Forms compute errors live, but a person is only told about one after they
  // leave the field or try to submit, never in the middle of a word.
  const [touched, setTouched] = useState(false);
  // Outside a form there is no submit to wait for, so errors show as given.
  const [inForm, setInForm] = useState(false);
  const error = !inForm || touched ? liveError : undefined;
  useEffect(() => {
    const form = wrapper.current?.closest("form");
    if (!form) return;
    setInForm(true);
    const onSubmit = () => setTouched(true);
    form.addEventListener("submit", onSubmit, true);
    return () => form.removeEventListener("submit", onSubmit, true);
  }, []);
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const describedBy = [hint ? hintId : null, error ? errorId : null].filter(Boolean).join(" ") || undefined;

  return (
    <div ref={wrapper} onBlur={() => setTouched(true)} className={cn("flex flex-col gap-1.5", className)}>
      <label htmlFor={id} className="text-sm font-medium">
        {label}
        {required && <span className="text-muted"> (required)</span>}
      </label>
      {children({ id, "aria-describedby": describedBy, "aria-invalid": error ? true : undefined })}
      {hint && (
        <p id={hintId} className="text-sm text-muted">
          {hint}
        </p>
      )}
      {error && (
        <p id={errorId} role="alert" className="flex items-start gap-1.5 text-sm font-medium">
          <Warning className="mt-0.5 shrink-0" aria-hidden />
          {error}
        </p>
      )}
    </div>
  );
}

export function Checkbox({
  label,
  className,
  ...rest
}: InputHTMLAttributes<HTMLInputElement> & { label: ReactNode }) {
  return (
    <label className={cn("flex cursor-pointer items-start gap-2 text-base", className)}>
      <input type="checkbox" className="mt-0.5 size-4 shrink-0 accent-(--ink)" {...rest} />
      <span>{label}</span>
    </label>
  );
}
