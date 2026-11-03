import type { Metadata } from "next";
import { notFound } from "next/navigation";
import { CopyButton } from "@/components/ui/copy-button";
import type { PublicRecord } from "@/features/domain/api";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { BACKEND_URL } from "@/server/auth";

// A publication's public page. It needs no session, and a DOI resolves here,
// so it renders on the server and search engines can read it.
async function load(slug: string): Promise<PublicRecord | null> {
  const res = await fetch(`${BACKEND_URL}/api/v1/public/publications/${encodeURIComponent(slug)}`, { next: { revalidate: 60 } });
  if (res.status === 404) return null;
  if (!res.ok) throw new Error(`publication ${res.status}`);
  return res.json();
}

export async function generateMetadata({ params }: { params: Promise<{ slug: string }> }): Promise<Metadata> {
  const { slug } = await params;
  const record = await load(slug);
  if (!record) return { title: "Not found" };
  return { title: record.title, description: record.description.slice(0, 200) || undefined };
}

// In UTC, so that the page says the same to everyone.
function day(iso: string) {
  return new Intl.DateTimeFormat("en", { dateStyle: "long", timeZone: "UTC" }).format(new Date(iso));
}

function Citation({ label, text }: { label: string; text: string }) {
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-base font-medium">{label}</h3>
        <CopyButton value={text} variant="ghost" />
      </div>
      <pre className="neo-in-sm mono-data overflow-x-auto p-3 text-sm whitespace-pre-wrap break-words" translate="no">
        {text}
      </pre>
    </div>
  );
}

export default async function PublishedPage({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  const r = await load(slug);
  if (!r) notFound();
  // "<" would let a title close this script tag.
  const jsonLd = JSON.stringify(r.schema_org).replace(/</g, "\\u003c");
  return (
    <main className="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-12">
      <script type="application/ld+json" dangerouslySetInnerHTML={{ __html: jsonLd }} />
      <header className="flex flex-col gap-3">
        <p className="text-base text-muted">
          Dataset, version {r.version}, {r.publication_year}
        </p>
        <h1 className="text-3xl font-semibold text-balance break-words">{r.title}</h1>
        <p className="text-base">
          {r.creators.map((c, i) => (
            <span key={c.name + i}>
              {i > 0 && ", "}
              {c.orcid ? (
                <a href={`https://orcid.org/${c.orcid}`} className="underline underline-offset-2">
                  {c.name}
                </a>
              ) : (
                c.name
              )}
              {c.affiliation && <span className="text-muted"> ({c.affiliation})</span>}
            </span>
          ))}
        </p>
        {r.doi && (
          <a href={`https://doi.org/${r.doi}`} className="mono-data w-fit break-all underline underline-offset-2">
            https://doi.org/{r.doi}
          </a>
        )}
      </header>

      {r.withdrawn_at && (
        <section className="neo-in-sm flex flex-col gap-1 p-4" role="status">
          <h2 className="text-base font-semibold">This dataset was withdrawn</h2>
          <p className="text-base break-words">{r.withdrawn_reason}</p>
          <p className="text-sm text-muted">
            Withdrawn <time dateTime={r.withdrawn_at}>{day(r.withdrawn_at)}</time>. This record stays so that citations keep resolving.
          </p>
        </section>
      )}

      {r.description && <p className="max-w-prose text-base break-words whitespace-pre-line">{r.description}</p>}

      <dl className="grid gap-x-6 gap-y-1 sm:grid-cols-[max-content_1fr] sm:gap-y-3">
        <dt className="text-muted">Publisher</dt>
        <dd className="mb-3 break-words sm:mb-0">{r.publisher}</dd>
        <dt className="text-muted">Licence</dt>
        <dd className="mb-3 break-words sm:mb-0">{r.license}</dd>
        <dt className="text-muted">Published</dt>
        <dd className="mb-3 sm:mb-0">
          <time dateTime={r.published_at}>{day(r.published_at)}</time>
        </dd>
        <dt className="text-muted">Contents</dt>
        <dd className="mb-3 tabular-nums sm:mb-0">
          {formatNumber(r.member_count)} files, {formatBytes(r.total_bytes)}
        </dd>
        <dt className="text-muted">Manifest SHA-256</dt>
        <dd className="mono-data break-all" translate="no">
          {r.manifest_sha256}
        </dd>
      </dl>
      <p className="max-w-prose text-base text-muted">
        The manifest hash identifies these exact files: any copy with the same hash is this dataset. The data itself is not public; ask the publisher for access.
      </p>

      <section className="flex flex-col gap-4">
        <h2 className="text-xl font-semibold">Cite this dataset</h2>
        <Citation label="Citation" text={r.citation} />
        <Citation label="BibTeX" text={r.bibtex} />
        <Citation label="RIS" text={r.ris} />
        <Citation label="DataCite metadata" text={JSON.stringify(r.datacite, null, 2)} />
      </section>
    </main>
  );
}
