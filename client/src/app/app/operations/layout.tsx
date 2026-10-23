import { PageHeader } from "@/components/ui/page";
import { OperationsTabs } from "@/features/operations/components/operations-tabs";

export default function OperationsLayout({ children }: LayoutProps<"/app/operations">) {
  return (
    <>
      <PageHeader
        title="Lifecycle and operations"
        description="Look up assets, manage retention and legal holds, and see what the API can tell you about platform health."
      />
      <OperationsTabs />
      {children}
    </>
  );
}
