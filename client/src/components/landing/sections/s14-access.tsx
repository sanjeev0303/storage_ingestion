import { AuditChain } from "../diagrams/audit-chain";
import { copy } from "../copy";
import { Section } from "../section";
import { AccessRoles } from "./access-roles";

export function Access() {
  return (
    <Section stop="access" tone="plum" labelledBy="access-title">
      <div data-reveal="left" className="max-w-3xl">
        <h2 id="access-title" className="t-h2">
          {copy.access.title}
        </h2>
        <p className="t-lead mt-6 max-w-[40ch]">{copy.access.text}</p>
      </div>
      <div className="mt-16 grid gap-16 lg:grid-cols-12 lg:gap-10">
        <div className="min-w-0 lg:col-span-7">
          <AccessRoles />
        </div>
        <div className="min-w-0 lg:sticky lg:top-24 lg:col-span-5 lg:self-start">
          <AuditChain />
        </div>
      </div>
    </Section>
  );
}
