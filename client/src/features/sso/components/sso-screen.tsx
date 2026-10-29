"use client";

import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/field";
import { PasswordInput } from "@/components/ui/password-field";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { useOrgMutation, useRolesQuery } from "@/features/organization/hooks";
import {
  deleteSso,
  getScimToken,
  getSso,
  issueScimToken,
  putSso,
  revokeScimToken,
  type SsoProtocol,
  type SsoSettings,
} from "../api";

export function SsoScreen() {
  const { organizationId, can } = useSession();
  const settings = useQuery({
    queryKey: orgKey(organizationId, "sso"),
    queryFn: ({ signal }) => getSso(signal),
    enabled: Boolean(organizationId),
  });

  if (settings.isPending) return <SkeletonRows rows={5} />;
  if (settings.isError) return <ErrorState error={settings.error} onRetry={() => settings.refetch()} />;

  return (
    <div className="flex flex-col gap-6">
      <SsoSettingsView key={settings.data.updated_at ?? "new"} current={settings.data} />
      {can("org.manage") && <ScimPanel />}
    </div>
  );
}

function SsoSettingsView({ current }: { current: SsoSettings }) {
  const oidcOn = current.redirect_uris.length > 0;
  const [protocol, setProtocol] = useState<SsoProtocol>(current.protocol ?? (current.saml && !oidcOn ? "SAML" : "OIDC"));

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <SsoForm current={current} protocol={protocol} onProtocol={setProtocol} />
      {protocol === "SAML" && current.saml ? (
        <Panel
          title="At your identity provider"
          description="Add a SAML 2.0 application with these two values, then paste the provider's metadata XML here."
        >
          <dl className="flex flex-col gap-3">
            <ProviderValue label="Entity ID (audience)" value={current.saml.entity_id} />
            <ProviderValue label="Assertion consumer service URL" value={current.saml.acs_url} />
          </dl>
          <p className="mt-4 text-sm">
            The response must be signed with the certificate in the metadata. Send the email address as the NameID or as an{" "}
            <span className="mono-data">email</span> attribute. Sign-ins started from the provider&apos;s own dashboard are refused.
          </p>
        </Panel>
      ) : (
        <Panel
          title="At your identity provider"
          description="Register an OpenID Connect web application with this callback address, then copy its issuer, client ID and secret here."
        >
          <ul className="flex flex-col gap-3">
            {current.redirect_uris.map((uri) => (
              <li key={uri} className="neo-in-sm flex flex-wrap items-center justify-between gap-2 px-3 py-2">
                <code className="mono-data break-all text-sm">{uri}</code>
                <CopyButton value={uri} variant="ghost" />
              </li>
            ))}
          </ul>
          <p className="mt-4 text-sm">
            Sign-in uses the authorization code flow with PKCE and asks for <span className="mono-data">openid email profile</span>.
            Only addresses the provider marks as verified, in the domains you list, are let in.
          </p>
        </Panel>
      )}
    </div>
  );
}

function ProviderValue({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-1">
      <dt className="text-sm font-medium">{label}</dt>
      <dd className="neo-in-sm flex flex-wrap items-center justify-between gap-2 px-3 py-2">
        <code className="mono-data break-all text-sm">{value}</code>
        <CopyButton value={value} variant="ghost" />
      </dd>
    </div>
  );
}

const PROTOCOLS: { id: SsoProtocol; label: string; help: string }[] = [
  { id: "OIDC", label: "OpenID Connect", help: "Okta, Entra ID, Google Workspace, Auth0, Keycloak and most modern providers." },
  { id: "SAML", label: "SAML 2.0", help: "For providers and company directories that only offer SAML, such as ADFS or older Okta apps." },
];

function SsoForm({
  current,
  protocol,
  onProtocol,
}: {
  current: SsoSettings;
  protocol: SsoProtocol;
  onProtocol: (p: SsoProtocol) => void;
}) {
  const { can } = useSession();
  const canManage = can("org.manage");
  const roles = useRolesQuery();
  const radioName = useId();
  const [metadata, setMetadata] = useState("");
  const [issuer, setIssuer] = useState(current.issuer ?? "");
  const [clientId, setClientId] = useState(current.client_id ?? "");
  const [secret, setSecret] = useState("");
  const [domains, setDomains] = useState(current.allowed_domains.join(", "));
  const [role, setRole] = useState(current.default_role);
  const [enabled, setEnabled] = useState(current.configured ? current.enabled : true);
  const [error, setError] = useState<string | null>(null);
  const [removing, setRemoving] = useState(false);
  const save = useOrgMutation(putSso, ["sso"]);
  const remove = useOrgMutation(deleteSso, ["sso"]);

  const domainList = domains
    .split(/[\s,]+/)
    .map((d) => d.trim().toLowerCase())
    .filter(Boolean);
  const saml = protocol === "SAML";
  // A switch between protocols starts the provider details over.
  const sameProtocol = current.configured && (current.protocol ?? "OIDC") === protocol;
  const issuerProblem = /^https?:\/\/\S+$/.test(issuer.trim()) ? "" : "Enter the issuer URL, starting with https://.";
  const secretProblem = !sameProtocol && !secret ? "Enter the client secret." : "";
  const metadataProblem = !metadata.trim() && !(sameProtocol && current.has_idp_metadata) ? "Paste the provider's metadata XML." : "";
  const providerValid = saml ? !metadataProblem : !issuerProblem && clientId.trim() !== "" && !secretProblem;
  const valid = providerValid && domainList.length > 0;

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!valid) return;
    setError(null);
    try {
      const shared = { protocol, allowed_domains: domainList, default_role: role, enabled };
      await save.mutateAsync(
        saml
          ? { ...shared, idp_metadata: metadata }
          : { ...shared, issuer: issuer.trim(), client_id: clientId.trim(), client_secret: secret },
      );
      setSecret("");
      setMetadata("");
      notify.success("Single sign-on saved");
    } catch (err) {
      setError(describeError(err));
    }
  }

  // Ownership is never given by a sign-in.
  const grantable = (roles.data?.roles ?? []).filter((r) => r.id !== "owner");

  return (
    <Panel
      className="lg:col-span-2"
      title="Single sign-on"
      description={
        current.configured ? (
          <>
            Last changed <Time value={current.updated_at} />. The provider is checked again on every save.
          </>
        ) : (
          "Let your people sign in through your own identity provider, over OpenID Connect or SAML 2.0."
        )
      }
      actions={
        current.configured && (
          <StateBadge tone={current.enabled ? "done" : "neutral"}>{current.enabled ? "In use" : "Turned off"}</StateBadge>
        )
      }
    >
      <form onSubmit={submit} noValidate className="flex flex-col gap-5">
        <fieldset disabled={!canManage} className="flex flex-col gap-5">
          <fieldset className="flex flex-col gap-3">
            <legend className="mb-1 text-sm font-medium">How your provider signs people in</legend>
            {PROTOCOLS.map((p) => {
              const off = p.id === "SAML" ? !current.saml : current.redirect_uris.length === 0;
              return (
                <label key={p.id} className={off ? "flex items-start gap-3 opacity-60" : "flex cursor-pointer items-start gap-3"}>
                  <input
                    type="radio"
                    name={radioName}
                    value={p.id}
                    checked={protocol === p.id}
                    disabled={off}
                    onChange={() => onProtocol(p.id)}
                    className="mt-1 size-4 shrink-0 accent-(--ink)"
                  />
                  <span>
                    <span className="block text-base font-medium">{p.label}</span>
                    <span className="block max-w-prose text-sm text-muted">
                      {off ? "Not turned on for this server." : p.help}
                    </span>
                  </span>
                </label>
              );
            })}
          </fieldset>
          {saml ? (
            <Field
              label="Identity provider metadata"
              required={!(sameProtocol && current.has_idp_metadata)}
              error={metadataProblem}
              hint={
                sameProtocol && current.has_idp_metadata
                  ? `Stored for ${current.issuer}. Leave empty to keep it, or paste new metadata after a certificate change.`
                  : "The XML your provider offers as SAML metadata. It names the provider, its sign-in address and its signing certificate."
              }
            >
              {(p) => (
                <Textarea
                  {...p}
                  rows={6}
                  value={metadata}
                  onChange={(e) => setMetadata(e.target.value)}
                  spellCheck={false}
                  className="mono-data text-sm"
                  placeholder="<EntityDescriptor xmlns=&quot;urn:oasis:names:tc:SAML:2.0:metadata&quot; …>"
                />
              )}
            </Field>
          ) : (
            <>
          <Field label="Issuer" required error={issuerProblem} hint="Its discovery document must name exactly this issuer.">
            {(p) => (
              <Input
                {...p}
                type="url"
                inputMode="url"
                spellCheck={false}
                value={issuer}
                onChange={(e) => setIssuer(e.target.value)}
                placeholder="https://login.example.com/"
                autoComplete="off"
              />
            )}
          </Field>
          <div className="grid gap-5 sm:grid-cols-2">
            <Field label="Client ID" required error={clientId.trim() ? "" : "Enter the client ID."}>
              {(p) => <Input {...p} value={clientId} onChange={(e) => setClientId(e.target.value)} autoComplete="off" spellCheck={false} />}
            </Field>
            <Field
              label="Client secret"
              required={!sameProtocol}
              error={secretProblem}
              hint={sameProtocol ? "Leave empty to keep the stored secret. It is never shown again." : "Stored sealed; never shown again."}
            >
              {(p) => <PasswordInput {...p} value={secret} onChange={(e) => setSecret(e.target.value)} autoComplete="new-password" />}
            </Field>
          </div>
            </>
          )}
          <Field
            label="Email domains allowed to sign in"
            required
            error={domainList.length ? "" : "Enter at least one domain."}
            hint="Separate them with commas, such as example.com, example.co.uk."
          >
            {(p) => <Textarea {...p} rows={2} value={domains} onChange={(e) => setDomains(e.target.value)} spellCheck={false} />}
          </Field>
          <Field
            label="People signing in for the first time"
            hint="With a role, their account and membership are made at their first sign-in. Accounts made this way have no password."
          >
            {(p) => (
              <Select {...p} value={role} onChange={(e) => setRole(e.target.value)}>
                <option value="">Only let in existing members</option>
                {grantable.map((r) => (
                  <option key={r.id} value={r.id}>
                    Join as {r.name}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Checkbox checked={enabled} onChange={(e) => setEnabled(e.target.checked)} label="Allow sign-in through this provider" />
        </fieldset>

        {error && (
          <p role="alert" className="text-base font-medium">
            {error}
          </p>
        )}
        {canManage ? (
          <div className="flex flex-wrap items-start justify-between gap-3">
            <SubmitButton
              valid={valid}
              loading={save.isPending}
              hint={saml ? "Paste the metadata and enter at least one domain." : "Fill in the issuer, client ID, secret and at least one domain."}
            >
              {current.configured ? "Save and check the provider" : "Turn on single sign-on"}
            </SubmitButton>
            {current.configured && (
              <Button type="button" onClick={() => setRemoving(true)}>
                Remove single sign-on
              </Button>
            )}
          </div>
        ) : (
          <p className="text-sm text-muted">Your role can see these settings but not change them (org.manage).</p>
        )}
      </form>

      <ConfirmDialog
        open={removing}
        onOpenChange={setRemoving}
        title="Remove single sign-on?"
        description="Nobody can sign in through the provider any more. People who only ever signed in that way have no password and lose access until they are given one."
        confirmLabel="Remove single sign-on"
        onConfirm={async () => {
          await remove.mutateAsync(undefined);
          notify.success("Single sign-on removed");
        }}
      />
    </Panel>
  );
}

function ScimPanel() {
  const { organizationId } = useSession();
  const status = useQuery({
    queryKey: orgKey(organizationId, "scim-token"),
    queryFn: ({ signal }) => getScimToken(signal),
    enabled: Boolean(organizationId),
  });
  const issue = useOrgMutation(issueScimToken, ["scim-token"]);
  const revoke = useOrgMutation(revokeScimToken, ["scim-token"]);
  const [token, setToken] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<"replace" | "revoke" | null>(null);
  const baseUrl = `${window.location.origin}/scim/v2`;

  async function makeToken() {
    const out = await issue.mutateAsync(undefined);
    setToken(out.token);
  }

  const enabled = status.data?.enabled ?? false;

  return (
    <Panel
      title="Directory sync (SCIM)"
      description="Let your identity provider add, suspend and remove members, and keep roles in step with its groups. Owners are never changed this way."
      actions={status.data && <StateBadge tone={enabled ? "done" : "neutral"}>{enabled ? "Token active" : "No token"}</StateBadge>}
    >
      {status.isPending ? (
        <SkeletonRows rows={2} />
      ) : status.isError ? (
        <ErrorState error={status.error} onRetry={() => status.refetch()} />
      ) : (
        <div className="grid gap-6 lg:grid-cols-2">
          <div className="flex flex-col gap-4">
            <dl className="flex flex-col gap-3">
              <ProviderValue label="SCIM base URL" value={baseUrl} />
            </dl>
            {enabled && (
              <p className="text-sm">
                Token made <Time value={status.data.created_at} />. Last used{" "}
                {status.data.last_used_at ? <Time value={status.data.last_used_at} /> : "never"}.
              </p>
            )}
            <div className="flex flex-wrap gap-3">
              <Button
                type="button"
                variant={enabled ? "secondary" : "primary"}
                disabled={issue.isPending}
                onClick={() => (enabled ? setConfirm("replace") : makeToken().catch(notify.error))}
              >
                {enabled ? "Replace token" : "Make a token"}
              </Button>
              {enabled && (
                <Button type="button" onClick={() => setConfirm("revoke")}>
                  Revoke token
                </Button>
              )}
            </div>
          </div>
          {token ? (
            <div className="neo-in flex flex-col gap-3 p-4" role="status">
              <p className="text-base font-medium">Copy this token into your provider now. It is not shown again.</p>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <code className="mono-data break-all text-sm">{token}</code>
                <CopyButton value={token} />
              </div>
              <Button type="button" variant="ghost" className="self-start" onClick={() => setToken(null)}>
                Hide the token
              </Button>
            </div>
          ) : (
            <ul className="flex list-disc flex-col gap-2 pl-5 text-sm">
              <li>New people join with the single sign-on default role, or as developers. Accounts made this way have no password.</li>
              <li>Each role is a group. Adding someone to a group gives them that role; removing them gives back the default.</li>
              <li>Deactivating someone suspends their membership. Deleting them removes it; their account stays.</li>
            </ul>
          )}
        </div>
      )}

      <ConfirmDialog
        open={confirm === "replace"}
        onOpenChange={(open) => setConfirm(open ? "replace" : null)}
        title="Replace the SCIM token?"
        description="The current token stops working at once. Directory sync fails until the new token is entered at your provider."
        confirmLabel="Replace token"
        onConfirm={makeToken}
      />
      <ConfirmDialog
        open={confirm === "revoke"}
        onOpenChange={(open) => setConfirm(open ? "revoke" : null)}
        title="Revoke the SCIM token?"
        description="Your provider can no longer change members. People already added stay as they are."
        confirmLabel="Revoke token"
        onConfirm={async () => {
          await revoke.mutateAsync(undefined);
          setToken(null);
          notify.success("SCIM token revoked");
        }}
      />
    </Panel>
  );
}
