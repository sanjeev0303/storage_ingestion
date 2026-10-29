"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/field";
import { readError } from "@/lib/api/errors";

function messageFor(code: string | undefined) {
  switch (code) {
    case "sso_not_configured":
      return "That organization has no single sign-on. Check its short name, or sign in with a password.";
    case "sso_disabled":
      return "Single sign-on is not enabled on this server.";
    case "rate_limited":
      return "Too many attempts. Try again shortly.";
    default:
      return "Single sign-on could not start. Try again.";
  }
}

// Sends the browser to the organization's identity provider.
export function SsoForm({ refused }: { refused?: string }) {
  const [organization, setOrganization] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(refused ?? null);

  async function start(e: FormEvent) {
    e.preventDefault();
    if (!organization.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const res = await fetch("/api/session/sso/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ organization: organization.trim() }),
        credentials: "same-origin",
      });
      if (!res.ok) {
        setError(messageFor((await readError(res)).code));
        setBusy(false);
        return;
      }
      const { authorization_url: url } = (await res.json()) as { authorization_url: string };
      window.location.assign(url);
    } catch {
      setError("Could not reach the server. Check the connection and try again.");
      setBusy(false);
    }
  }

  return (
    <form onSubmit={start} noValidate className="flex flex-col gap-4">
      <Field label="Organization short name" hint="The part of your organization's address an administrator gave you, such as acme-3f9c.">
        {(p) => (
          <Input {...p} value={organization} onChange={(e) => setOrganization(e.target.value)} autoCapitalize="none" spellCheck={false} />
        )}
      </Field>
      {error && (
        <p role="alert" className="text-base font-medium">
          {error}
        </p>
      )}
      <Button type="submit" className="h-10 self-start" loading={busy} disabled={!organization.trim()}>
        Continue with single sign-on
      </Button>
    </form>
  );
}
