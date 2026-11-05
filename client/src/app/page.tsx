import { BarcodeBand } from "@/components/home/barcode-band";
import { DepthSwitch } from "@/components/home/depth-switch";
import { FeatureBoard } from "@/components/home/feature-board";
import { FeatureIndex } from "@/components/home/feature-index";
import { FixChapters } from "@/components/home/fix-chapters";
import { FooterMark } from "@/components/home/footer-mark";
import { Hero } from "@/components/home/hero";
import { HomeHeader } from "@/components/home/home-header";
import { HomeMotion } from "@/components/home/home-motion";
import { MapSection } from "@/components/home/map-section";
import { ReadinessDeck } from "@/components/home/readiness-deck";
import { StageBar } from "@/components/home/stage-bar";
import { StartBand } from "@/components/home/start-band";
import { StorageTiles } from "@/components/home/storage-tiles";

// Datasets handled the way a lab accessions specimens: received, tested,
// labelled, and released or rejected, with the label saying what was never
// tested. One motif runs the page: the accession label with its cut corner.
export default function Home() {
  return (
    <HomeMotion>
      <HomeHeader />
      <main id="main">
        <Hero />
        <DepthSwitch />
        <StageBar />
        <FeatureBoard />
        <MapSection />
        <BarcodeBand />
        <FixChapters />
        <FeatureIndex />
        <ReadinessDeck />
        <StorageTiles />
        <StartBand />
      </main>
      <FooterMark />
    </HomeMotion>
  );
}
