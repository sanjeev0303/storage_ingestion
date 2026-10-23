"use client";

import { ButtonLink } from "@/components/ui/button";
import { DefList, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useProfile } from "../hooks";
import type { ValidationProfile } from "../types";
import { SectionHeading } from "./parts";

export function ProfileDetail({ profileId, version }: { profileId: string; version?: number }) {
  const profile = useProfile(profileId, version);
  if (profile.isPending) return <SkeletonRows rows={6} />;
  if (profile.isError) return <ErrorState error={profile.error} onRetry={() => profile.refetch()} />;
  return <ProfileView profile={profile.data} />;
}

function formatValue(value: unknown): string {
  if (Array.isArray(value)) return value.map(formatValue).join(", ");
  if (value !== null && typeof value === "object") return JSON.stringify(value);
  return String(value);
}

function ProfileView({ profile }: { profile: ValidationProfile }) {
  const def = profile.definition;
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        crumbs={[{ label: "Validation", href: "/app/validation" }, { label: profile.name }]}
        title={profile.name}
        description={profile.description || "No description."}
        actions={<ButtonLink href="/app/validation">All profiles</ButtonLink>}
      />
      <Panel title="Profile">
        <DefList
          items={[
            { label: "Id", value: <span className="mono-data text-sm">{profile.id}</span> },
            { label: "Version", value: profile.version },
            { label: "Origin", value: <StateBadge>{profile.built_in ? "Built in" : "Organization"}</StateBadge> },
            { label: "Kind", value: def.kind || "Not set" },
            { label: "Created", value: profile.built_in ? "Ships with the platform" : <Time value={profile.created_at} /> },
            {
              label: "Thresholds",
              value: `Failed above ${def.thresholds.max_errors} errors; ${
                def.thresholds.max_warnings === undefined ? "no limit on warnings" : `failed above ${def.thresholds.max_warnings} warnings`
              }`,
            },
          ]}
        />
        <p className="mt-4 max-w-prose text-sm text-muted">
          Info findings never count. More errors than the limit make a version Failed; otherwise any error or warning makes it Ready with warnings.
        </p>
      </Panel>

      <section aria-labelledby="rules-heading" className="flex flex-col gap-3">
        <SectionHeading id="rules-heading">Rules ({(def.rules ?? []).length})</SectionHeading>
        <Table aria-label="Rules">
          <thead>
            <tr>
              <Th>Rule id</Th>
              <Th>Type</Th>
              <Th>Severity</Th>
              <Th>Scope</Th>
              <Th>Enabled</Th>
              <Th>Config</Th>
            </tr>
          </thead>
          <tbody>
            {(def.rules ?? []).map((rule) => (
              <tr key={rule.id}>
                <Td>
                  <span className="mono-data text-sm">{rule.id}</span>
                  <span className="text-xs text-muted"> v{rule.version}</span>
                </Td>
                <Td>
                  <span className="mono-data text-sm">{rule.type}</span>
                </Td>
                <Td>{rule.severity.charAt(0) + rule.severity.slice(1).toLowerCase()}</Td>
                <Td>{rule.scope.toLowerCase().replace("_", " ")}</Td>
                <Td>{rule.enabled ? "Yes" : "No, skipped"}</Td>
                <Td className="min-w-48 whitespace-normal">
                  {rule.config && Object.keys(rule.config).length > 0 ? (
                    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-sm">
                      {Object.entries(rule.config).map(([key, value]) => (
                        <div key={key} className="contents">
                          <dt className="mono-data text-muted">{key}</dt>
                          <dd className="[overflow-wrap:anywhere]">{formatValue(value)}</dd>
                        </div>
                      ))}
                    </dl>
                  ) : (
                    <span className="text-muted">Defaults</span>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      </section>
    </div>
  );
}
