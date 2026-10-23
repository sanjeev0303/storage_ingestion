import type { Metadata, Viewport } from "next";
import { Hanken_Grotesk, JetBrains_Mono } from "next/font/google";
import { ErrorReporter } from "@/components/error-reporter";
import { SmoothScroll } from "@/components/smooth-scroll";
import { Toaster } from "@/components/ui/toast";
import { QueryProvider } from "@/lib/query/provider";
import "./globals.css";

const hanken = Hanken_Grotesk({ variable: "--font-hanken", subsets: ["latin"], display: "swap" });
const jetbrains = JetBrains_Mono({ variable: "--font-jetbrains", subsets: ["latin"], display: "swap" });

export const metadata: Metadata = {
  title: { default: "Object Storage", template: "%s | Object Storage" },
  description:
    "Connect storage, ingest datasets, and see exactly what was verified before a snapshot is used for training.",
};

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#DDE3E8" },
    { media: "(prefers-color-scheme: dark)", color: "#3A0F34" },
  ],
};

// Applies the saved theme before first paint so the page never flashes the wrong one.
const themeScript = `try{var t=localStorage.getItem("osp.theme");if(t==="light"||t==="dark")document.documentElement.dataset.theme=t}catch(e){}`;

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en" suppressHydrationWarning className={`${hanken.variable} ${jetbrains.variable}`}>
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
