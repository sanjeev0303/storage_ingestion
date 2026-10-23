"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { z } from "zod";
import { Field, Input } from "@/components/ui/field";
import { PasswordInput, PasswordRules } from "@/components/ui/password-field";
import { SubmitButton } from "@/components/ui/submit-button";
import { Panel } from "@/components/ui/panel";
import { describeError, readError } from "@/lib/api/errors";
import { PASSWORD_MIN_LENGTH, passwordRules } from "@/lib/validation/password";
import { zodResolver } from "@/lib/validation/resolver";

const schema = z.object({
  organization_name: z.string().trim().min(1, "Enter a name for your organization.").max(200, "Use 200 characters or fewer."),
  display_name: z.string().trim().max(200, "Use 200 characters or fewer."),
  email: z.string().trim().toLowerCase().min(1, "Enter your email address.").email("Enter a valid email address."),
  password: z.string().min(PASSWORD_MIN_LENGTH, `Use at least ${PASSWORD_MIN_LENGTH} characters.`),
});
type Values = z.infer<typeof schema>;

function messageFor(code: string | undefined, fallback: string, retryAfter?: number) {
  switch (code) {
    case "registration_closed":
      return "Sign-up is not open on this platform. Ask an administrator for an invitation.";
    case "conflict":
      return "An account with this email already exists. Sign in instead.";
    case "rate_limited":
      return retryAfter ? `Too many attempts. Try again in ${retryAfter} seconds.` : "Too many attempts. Try again shortly.";
    default:
      return fallback;
  }
}

export function RegisterForm() {
  const router = useRouter();
  const [formError, setFormError] = useState<string | null>(null);
  const {
    register,
    handleSubmit,
    control,
    formState: { errors, isSubmitting, isValid },
  } = useForm<Values>({
    resolver: zodResolver<Values>(schema),
    mode: "onTouched",
    defaultValues: { organization_name: "", display_name: "", email: "", password: "" },
  });

  const [password, email] = useWatch({ control, name: ["password", "email"] });
  const rules = passwordRules(password, { email });
  const rulesMet = rules.every((rule) => rule.met);

  async function onSubmit(values: Values) {
    setFormError(null);
    try {
      const res = await fetch("/api/session/register", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(values),
        credentials: "same-origin",
      });
      if (!res.ok) {
        const error = await readError(res);
        setFormError(messageFor(error.code, describeError(error), error.retryAfter));
        return;
      }
      router.replace("/app/overview");
    } catch {
      setFormError("Could not reach the server. Check the connection and try again.");
    }
  }

  return (
    <Panel as="div" className="w-full max-w-md" padded>
      <h1 className="text-2xl font-semibold tracking-tight">Create an account</h1>
      <p className="mt-1 text-base text-muted">You become the owner of a new organization and can invite others to it.</p>

      <form onSubmit={handleSubmit(onSubmit)} noValidate className="mt-6 flex flex-col gap-5">
        <Field label="Organization name" required error={errors.organization_name?.message}>
          {(p) => <Input autoComplete="organization" maxLength={200} autoFocus {...p} {...register("organization_name")} />}
        </Field>
        <Field label="Your name" hint="Optional." error={errors.display_name?.message}>
          {(p) => <Input autoComplete="name" maxLength={200} {...p} {...register("display_name")} />}
        </Field>
        <Field label="Email" required error={errors.email?.message}>
          {(p) => <Input type="email" autoComplete="username" autoCapitalize="none" spellCheck={false} {...p} {...register("email")} />}
        </Field>
        <Field label="Password" required error={errors.password?.message}>
          {(p) => <PasswordInput autoComplete="new-password" {...p} {...register("password")} />}
        </Field>
        <PasswordRules rules={rules} />

        {formError && (
          <p role="alert" className="text-base font-medium">
            {formError}
          </p>
        )}

        <SubmitButton valid={isValid && rulesMet} loading={isSubmitting} className="h-10">
          Create account
        </SubmitButton>
      </form>

      <p className="mt-6 text-sm text-muted">
        Already have an account?{" "}
        <Link href="/login" className="font-medium text-ink underline underline-offset-2">
          Sign in
        </Link>
        . Have an invitation?{" "}
        <Link href="/accept-invitation" className="font-medium text-ink underline underline-offset-2">
          Accept it
        </Link>
        .
      </p>
    </Panel>
  );
}
