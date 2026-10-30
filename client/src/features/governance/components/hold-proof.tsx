"use client";

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Modal } from "@/components/ui/dialog";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { holdProof } from "../api";

// The assets a hold covers, when each one's lock took effect, and, when
// asked, what the provider holds now.
export function HoldProofDialog({ holdId, title, onClose }: { holdId: string; title: string; onClose: () => void }) {
  const { organizationId } = useSession();
  const [verify, setVerify] = useState(false);
  const proof = useQuery({
    queryKey: orgKey(organizationId, "retention", "proof", holdId, verify),
    queryFn: ({ signal }) => holdProof(holdId, verify, signal),
    enabled: Boolean(organizationId),
  });

  return (
    <Modal
      open
      onOpenChange={(open) => !open && onClose()}
      title={`What ${title} holds`}
      description="Each asset the hold covers, and when the platform set its Object Lock. Reading the locks back asks the provider itself."
      footer={
        <Button onClick={() => setVerify(true)} disabled={verify} loading={verify && proof.isFetching}>
          Read locks back from the provider
        </Button>
      }
    >
      {proof.isPending ? (
        <SkeletonRows rows={4} />
      ) : proof.isError ? (
        <ErrorState error={proof.error} onRetry={() => proof.refetch()} />
      ) : proof.data.assets.length === 0 ? (
        <p className="text-base text-muted">The hold covers no asset yet. Assets that match later are covered as they arrive.</p>
      ) : (
        <div className="max-h-[50vh] overflow-auto" data-lenis-prevent>
          <Table>
            <caption className="sr-only">Assets the hold covers</caption>
            <thead>
              <tr>
                <Th>Asset</Th>
                <Th>Lock</Th>
                {verify && <Th>At the provider</Th>}
              </tr>
            </thead>
            <tbody>
              {proof.data.assets.map((a) => (
                <tr key={a.asset_id}>
                  <Td className="max-w-xs">
                    <div className="[overflow-wrap:anywhere]">{a.file_name}</div>
                    <div translate="no" className="mono-data text-sm text-muted [overflow-wrap:anywhere]">
                      {a.asset_id}
                    </div>
                  </Td>
                  <Td>
                    {a.lock_applied_at ? (
                      <span className="text-sm">
                        {a.lock_legal_hold ? "Legal hold" : "No hold"}
                        {a.lock_retain_until && (
                          <>
                            , {a.lock_mode?.toLowerCase() ?? "governance"} until <Time value={a.lock_retain_until} />
                          </>
                        )}
                        <span className="block text-muted">
                          set <Time value={a.lock_applied_at} />
                        </span>
                      </span>
                    ) : (
                      <span className="text-sm text-muted">No Object Lock on its bucket</span>
                    )}
                  </Td>
                  {verify && (
                    <Td>
                      {a.provider ? (
                        <StateBadge tone={a.provider === "MATCH" ? "done" : a.provider.startsWith("MISMATCH") ? "failed" : "warning"}>
                          {a.provider === "MATCH" ? "Matches" : a.provider}
                        </StateBadge>
                      ) : (
                        <span className="text-sm text-muted">Not read</span>
                      )}
                    </Td>
                  )}
                </tr>
              ))}
            </tbody>
          </Table>
          {proof.data.truncated && <p className="mt-2 text-sm text-muted">The first 1,000 assets are listed.</p>}
        </div>
      )}
    </Modal>
  );
}
