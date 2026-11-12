import type { Metadata, Viewport } from "next";
import { Archivo, JetBrains_Mono } from "next/font/google";
import { ErrorReporter } from "@/components/error-reporter";
import { SmoothScroll } from "@/components/smooth-scroll";
import { Toaster } from "@/components/ui/toast";
import { QueryProvider } from "@/lib/query/provider";
import "./globals.css";

// Archivo with its width axis: normal width for text, expanded for headings and the wordmark.
const archivo = Archivo({ variable: "--font-archivo", subsets: ["latin"], axes: ["wdth"], display: "swap" });
const jetbrains = JetBrains_Mono({ variable: "--font-jetbrains", subsets: ["latin"], display: "swap" });

export const metadata: Metadata = {
  // Absolute URLs for the share image; CONSOLE_ORIGIN is the console's public address (also used for SSO).
  metadataBase: new URL(process.env.CONSOLE_ORIGIN ?? "http://localhost:3100"),
  title: { default: "Orbidune: verified dataset storage for ML teams", template: "%s | Orbidune" },
  description:
    "Orbidune hashes and scans every upload before anyone can read it, pins exact versions in dataset snapshots, and reports what was checked and what was not.",
  applicationName: "Orbidune",
};

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#FFFFFF" },
    { media: "(prefers-color-scheme: dark)", color: "#0E112B" },
  ],
};

// Applies the saved theme before first paint so the page never flashes the wrong one.
const themeScript = `try{var t=localStorage.getItem("orb.theme");if(t==="light"||t==="dark")document.documentElement.dataset.theme=t}catch(e){}`;

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en" suppressHydrationWarning className={`${archivo.variable} ${jetbrains.variable}`}>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeScript }} />
      </head>
      <body>
        <a
          href="#main"
          className="neo-sm fixed top-2 left-2 z-[60] -translate-y-16 px-4 py-2 font-medium focus:translate-y-0"
        >
          Skip to content
        </a>
        <QueryProvider>
          <ErrorReporter />
          <SmoothScroll />
          {children}
          <Toaster />
        </QueryProvider>
      </body>
    </html>
  );
}
