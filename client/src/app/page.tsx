import { Closing } from "@/components/home/closing";
import { Features } from "@/components/home/features";
import { Hero } from "@/components/home/hero";
import { HomeHeader } from "@/components/home/home-header";
import { Integrations } from "@/components/home/integrations";
import { Method } from "@/components/home/method";
import { Problem } from "@/components/home/problem";
import { Report } from "@/components/home/report";
import { Solved } from "@/components/home/solved";

// Orbidune presented the way its readers read evidence: an abstract with a live
// figure, the failures it removes, the method, a results table, then the details.
export default function Home() {
  return (
    <>
      <HomeHeader />
      <Hero />
      <div className="orbit-page">
        <span aria-hidden className="orbit-line" />
        <main id="main">
          <Problem />
          <Solved />
          <Method />
          <Report />
          <Features />
          <Integrations />
        </main>
      </div>
      <Closing />
    </>
  );
}
