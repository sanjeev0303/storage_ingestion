import Link from "next/link";
import { buttonClass } from "@/components/ui/button";

export default function NotFound() {
  return (
    <div className="neo-in mx-auto my-12 flex max-w-xl flex-col items-start gap-4 p-8">
      <h1 className="text-xl font-semibold">That page does not exist.</h1>
      <p className="text-base text-muted">The address may be mistyped, or the page was moved or removed.</p>
      <p className="text-base font-medium">Go to the overview, or sign in again if you followed an old link.</p>
      <div className="flex flex-wrap gap-2">
        <Link href="/app/overview" className={buttonClass("primary")}>
          Go to the overview
        </Link>
        <Link href="/" className={buttonClass("secondary")}>
          Home page
        </Link>
      </div>
    </div>
  );
}
