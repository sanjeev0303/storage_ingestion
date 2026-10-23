"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useEffect, useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { z } from "zod";
import { Field, Input } from "@/components/ui/field";
import { PasswordInput, PasswordRules } from "@/components/ui/password-field";
import { SubmitButton } from "@/components/ui/submit-button";
import { Panel } from "@/components/ui/panel";
import { api } from "@/lib/api/client";
import { describeError } from "@/lib/api/errors";
import { passwordRules } from "@/lib/validation/password";
import { zodResolver } from "@/lib/validation/resolver";

// One form for the three flows that redeem a token the platform gave out:
// activating an account, resetting a password, and accepting an invitation.
// None of them sign the person in.
type Flow = {
  title: string;
  intro: string;
  path: string;
  button: string;
  done: string;
  withName?: boolean;
  passwordOptional?: boolean;
};

const schema = z.object({
  token: z.string().trim().min(1, "Paste the token you were given."),
  password: z.string(),
  display_name: z.string().trim().optional(),
});
type Values = z.infer<typeof schema>;

export function TokenForm({ flow }: { flow: Flow }) {
  const params = useSearchParams();
  const [done, setDone] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const {
    register,
    handleSubmit,
    setValue,
    control,
    formState: { errors, isSubmitting, isValid },
  } = useForm<Values>({
    resolver: zodResolver<Values>(schema),
    mode: "onTouched",
    defaultValues: { token: "", password: "", display_name: "" },
  });

  // A link can carry the token. Take it into the form and drop it from the address bar.
  useEffect(() => {
    const token = params.get("token");
    if (!token) return;
    setValue("token", token);
    window.history.replaceState(null, "", window.location.pathname);
  }, [params, setValue]);

  const password = useWatch({ control, name: "password" });
  const rules = passwordRules(password);
  // An invitation can be accepted without a password when already signed in.
  const passwordOk = flow.passwordOptional && password === "" ? true : rules.every((rule) => rule.met);

  async function onSubmit(values: Values) {
    setFormError(null);
    try {
      await api(flow.path, {
        method: "POST",
        org: false,
        body: {
          token: values.token,
          ...(values.password ? { password: values.password } : {}),
          ...(flow.withName && values.display_name ? { display_name: values.display_name } : {}),
        },
      });
      setDone(true);
    } catch (error) {
      setFormError(describeError(error));
    }
  }

  if (done) {
    return (
      <Panel as="div" className="w-full max-w-md">
        <h1 className="text-2xl font-semibold tracking-tight">{flow.done}</h1>
        <p className="mt-2 text-base text-muted">You can sign in now.</p>
        <Link href="/login" className="mt-6 inline-block font-medium underline underline-offset-2">
          Go to sign in
        </Link>
      </Panel>
    );
  }

  return (
    <Panel as="div" className="w-full max-w-md">
      <h1 className="text-2xl font-semibold tracking-tight">{flow.title}</h1>
      <p className="mt-1 text-base text-muted">{flow.intro}</p>

      <form onSubmit={handleSubmit(onSubmit)} noValidate className="mt-6 flex flex-col gap-5">
        <Field label="Token" required error={errors.token?.message}>
          {(p) => <Input autoComplete="off" spellCheck={false} {...p} {...register("token")} />}
        </Field>
        {flow.withName && (
          <Field label="Display name" hint="Optional. Ignored if you are already signed in.">
            {(p) => <Input autoComplete="name" maxLength={200} {...p} {...register("display_name")} />}
          </Field>
        )}
        <Field
          label={flow.passwordOptional ? "Password for a new account" : "New password"}
          required={!flow.passwordOptional}
          hint={flow.passwordOptional ? "Leave empty if you are already signed in and joining with your account." : undefined}
          error={errors.password?.message}
        >
          {(p) => <PasswordInput autoComplete="new-password" {...p} {...register("password")} />}
        </Field>
        {(!flow.passwordOptional || password !== "") && <PasswordRules rules={rules} />}

        {formError && (
          <p role="alert" className="text-base font-medium">
            {formError}
          </p>
        )}

        <SubmitButton valid={isValid && passwordOk} loading={isSubmitting} className="h-10">
          {flow.button}
        </SubmitButton>
      </form>

      <Link href="/login" className="mt-6 inline-block text-sm text-muted underline underline-offset-2">
        Back to sign in
      </Link>
    </Panel>
  );
}
