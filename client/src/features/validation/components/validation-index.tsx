"use client";

import Link from "next/link";
import { ButtonLink } from "@/components/ui/button";
import { LoadMore, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { useDatasetPicker, useProfiles } from "../hooks";
import { SectionHeading } from "./parts";

export function ValidationIndex() {
  const { can } = useSession();
  return (
    <div className="flex flex-col gap-8">
      <PageHeader
        title="Validation"
        description="Check a dataset snapshot against a profile and read what was, and was not, examined."
        actions={can("datasets.manage") ? <ButtonLink href="/app/validation/profiles/new" variant="primary">New profile</ButtonLink> : null}
      />

      <div className="grid gap-6 lg:grid-cols-3">
        <Panel title="What validation proves" className="lg:col-span-2">
          <div className="flex max-w-prose flex-col gap-3 text-base">
            <p>
              A validation runs a profile&apos;s rules over one immutable snapshot. The report says what was checked, how (metadata only, every
              member&apos;s content, or a sample), what was not checked, and the evidence behind each answer.
            </p>
            <p>
              The verdict is Ready, Ready with warnings or Failed, decided by the profile&apos;s thresholds on errors and warnings. There is no
              score and no readiness percentage.
            </p>
            <p className="font-medium">
              Passing does not prove the data is semantically correct, that labels are right, that it is legally cleared, or that a model will train
              well on it. Metadata-only validation never reads object content at all.
            </p>
          </div>
        </Panel>
        <Panel title="Where reports live">
          <p className="text-base text-muted">
            The API has no list of reports across datasets. Open a dataset to start a check and to see its jobs and reports. A report link from a job
            or from lineage opens it directly.
          </p>
        </Panel>
      </div>

      <ProfilesSection />
      <DatasetsSection />
    </div>
  );
}

function ProfilesSection() {
  const profiles = useProfiles();
  return (
    <section aria-labelledby="profiles-heading" className="flex flex-col gap-3">
      <SectionHeading id="profiles-heading">Profiles</SectionHeading>
      <p className="max-w-prose text-sm text-muted">
        A profile is a versioned list of rules with thresholds. Built-in profiles ship with the platform. Creating a profile with an existing name
        adds a new version; older versions never change.
      </p>
      {profiles.isPending ? (
        <SkeletonRows rows={4} />
      ) : profiles.isError ? (
        <ErrorState error={profiles.error} onRetry={() => profiles.refetch()} />
      ) : profiles.data.profiles.length === 0 ? (
        <EmptyState title="No profiles">Built-in profiles should always be present. If this list is empty, reload; if it stays empty, contact support.</EmptyState>
      ) : (
        <>
          <Table aria-label="Validation profiles">
            <thead>
              <tr>
                <Th>Name</Th>
                <Th>Id</Th>
                <Th>Version</Th>
                <Th>Origin</Th>
                <Th>Kind</Th>
                <Th>Rules</Th>
                <Th>Created</Th>
              </tr>
            </thead>
            <tbody>
              {profiles.data.profiles.map((p) => (
                <tr key={`${p.id}@${p.version}`}>
                  <Td>
                    <Link href={`/app/validation/profiles/${encodeURIComponent(p.id)}?version=${p.version}`} className="font-medium underline underline-offset-2">
                      {p.name}
                    </Link>
                  </Td>
                  <Td>
                    <span className="mono-data text-sm">{p.id}</span>
                  </Td>
                  <Td>{p.version}</Td>
                  <Td>
                    <StateBadge tone={p.built_in ? "neutral" : "done"}>{p.built_in ? "Built in" : "Organization"}</StateBadge>
                  </Td>
                  <Td>{p.definition.kind || "Not set"}</Td>
                  <Td>{p.definition.rules?.length ?? 0}</Td>
                  <Td>
                    <Time value={p.created_at} />
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <p className="text-sm text-muted">
            Rule types the server accepts: <span className="mono-data text-xs [overflow-wrap:anywhere]">{profiles.data.rule_types.join(", ")}</span>
          </p>
        </>
      )}
    </section>
  );
}

function DatasetsSection() {
  const datasets = useDatasetPicker();
  return (
    <section aria-labelledby="datasets-heading" className="flex flex-col gap-3">
      <SectionHeading id="datasets-heading">Check a dataset</SectionHeading>
      {datasets.isPending ? (
        <SkeletonRows rows={4} />
      ) : datasets.isError ? (
        <ErrorState error={datasets.error} onRetry={() => datasets.refetch()} />
      ) : datasets.items.length === 0 ? (
        <EmptyState
          title="No datasets yet"
          action={<ButtonLink href="/app/datasets">Go to datasets</ButtonLink>}
        >
          Validation runs against a dataset snapshot. Create a dataset, add members and take a snapshot, then come back or use the dataset&apos;s own
          Validation tab.
        </EmptyState>
      ) : (
        <>
          <Table aria-label="Datasets">
            <thead>
              <tr>
                <Th>Dataset</Th>
                <Th>Lifecycle</Th>
                <Th>Latest report</Th>
                <Th>Checks</Th>
              </tr>
            </thead>
            <tbody>
              {datasets.items.map((d) => (
                <tr key={d.id}>
                  <Td className="font-medium">{d.name}</Td>
                  <Td>{d.lifecycle_status}</Td>
                  <Td>
                    {d.latest_report_id ? (
                      <Link href={`/app/validation/reports/${d.latest_report_id}`} className="underline underline-offset-2">
                        Open
                      </Link>
                    ) : (
                      <span className="text-muted">None yet</span>
                    )}
                  </Td>
                  <Td>
                    <Link href={`/app/datasets/${d.id}/validation`} className="underline underline-offset-2">
                      Validation and readiness
                    </Link>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore hasNext={Boolean(datasets.hasNextPage)} loading={datasets.isFetchingNextPage} onLoad={() => datasets.fetchNextPage()} shown={datasets.items.length} />
        </>
      )}
    </section>
  );
}
