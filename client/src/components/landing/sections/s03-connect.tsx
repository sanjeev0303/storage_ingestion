import { Outcome, StateBadge } from "@/components/ui/status";
import { ConnectionLifecycle } from "../diagrams/connection-lifecycle";
import { copy } from "../copy";
import { Section } from "../section";

// One example connection row, shown in the state the step leaves it in.
const rowStates = [
  { label: "Pending", badges: <StateBadge>Pending</StateBadge> },
  {
    label: "Active",
    badges: (
      <>
        <StateBadge tone="active">Active</StateBadge>
        <Outcome outcome="passed" label="Test passed" />
      </>
    ),
  },
  {
    label: "Active and default",
    badges: (
      <>
        <StateBadge tone="active">Active</StateBadge>
        <StateBadge tone="done">Default</StateBadge>
      </>
    ),
  },
];

export function Connect() {
  return (
    <Section stop="connect" tone="ground" labelledBy="connect-title">
      <div className="grid gap-16 lg:grid-cols-12 lg:gap-x-8">
        <div data-reveal="left" className="lg:col-span-6">
          <h2 id="connect-title" className="t-h2">
            {copy.connect.title}
          </h2>
          <p className="t-body mt-6 max-w-[56ch]">{copy.connect.text}</p>
          <ul className="t-body mt-6 flex flex-wrap gap-x-6 gap-y-1 text-muted">
            {copy.connect.providers.map((provider) => (
              <li key={provider}>{provider}</li>
            ))}
          </ul>

          <ol className="mt-16 flex flex-col gap-14 lg:mt-[88px] lg:gap-[88px]">
            {copy.connect.steps.map((step, index) => (
              <li key={step.order}>
                <p className="t-body max-w-[52ch]">
                  <span className="font-medium">{step.order}</span> {step.text}
                </p>
                <div
                  data-ui
                  role="group"
                  aria-label={`Example connection, ${rowStates[index].label}`}
                  className="neo-sm mt-4 flex flex-wrap items-center gap-x-4 gap-y-2 bg-ground p-3 text-ink"
                >
                  <span className="font-medium">training-archive</span>
                  <span className="t-small text-muted">Amazon S3</span>
                  <span className="ml-auto flex flex-wrap items-center gap-3">{rowStates[index].badges}</span>
                </div>
              </li>
            ))}
          </ol>
        </div>

        <div data-reveal="right" className="lg:col-span-5 lg:col-start-8">
          <div className="lg:sticky lg:top-24">
            <ConnectionLifecycle />
          </div>
        </div>
      </div>
    </Section>
  );
}
