package evidence

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// custodyLine is one member's chain of custody in an evidence pack.
type custodyLine struct {
	Ordinal     int64      `json:"ordinal"`
	ControlNo   string     `json:"control_number"`
	Path        string     `json:"path"`
	AssetID     string     `json:"version_id"`
	ObjectID    string     `json:"object_id"`
	Size        int64      `json:"size_bytes"`
	ContentType string     `json:"content_type"`
	SHA256      string     `json:"sha256"`
	MD5         string     `json:"md5,omitempty"`
	SHA1        string     `json:"sha1,omitempty"`
	UploadedBy  string     `json:"uploaded_by"`
	UploadedAt  *time.Time `json:"uploaded_at,omitempty"`
	Connection  string     `json:"storage_connection_id"`
	StorageVer  string     `json:"storage_version_id,omitempty"`
	Tier        string     `json:"tier"`
	Lock        any        `json:"object_lock,omitempty"`
	Status      string     `json:"status"`
	Reads       any        `json:"reads"`
}

var controlPrefix = regexp.MustCompile(`[^A-Z0-9]+`)

// PackOptions choose what an evidence pack holds.
type PackOptions struct {
	// Digests reads every member once to add MD5 and SHA-1 (kept for next
	// time); without it, only digests already known are included.
	Digests bool
	// Reads is how many reads of each member to list (default 100).
	Reads int
}

// WritePack writes a snapshot's evidence pack, a zip, to w: its manifest
// and signed manifest, each member's chain of custody (who brought it in,
// every read, its lock), the audit chain's head and latest anchor, DAT and
// OPT load files for review tools, a verification script, and
// SHA256SUMS. Authorization is checked before the first byte; a failure
// after that leaves the zip unfinished.
func (s *Service) WritePack(ctx context.Context, p model.Principal, datasetID, versionID string, opts PackOptions, w io.Writer) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "dataset.evidence_pack", "dataset_version", versionID); err != nil {
		return err
	}
	ds, err := s.deps.Datasets.Get(ctx, p, datasetID)
	if err != nil {
		return err
	}
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return err
	}
	if opts.Reads <= 0 || opts.Reads > 1000 {
		opts.Reads = 100
	}
	signed, err := s.deps.Streaming.SignedManifest(ctx, p, datasetID, versionID)
	if err != nil {
		return err
	}
	rows, err := s.deps.Store.Custody(ctx, p.TenantID, versionID)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.AssetID)
	}
	digests, err := s.deps.Store.Digests(ctx, ids)
	if err != nil {
		return err
	}
	if opts.Digests {
		if err := s.computeDigests(ctx, p, rows, digests); err != nil {
			return err
		}
	}
	s.record(ctx, p, "dataset.evidence_pack", "dataset_version", versionID, map[string]any{"members": len(rows), "digests": opts.Digests})

	sums := map[string]string{}
	z := zip.NewWriter(w)
	add := func(name string, write func(io.Writer) error) error {
		f, err := z.Create(name)
		if err != nil {
			return err
		}
		h := sha256.New()
		if err := write(io.MultiWriter(f, h)); err != nil {
			return err
		}
		sums[name] = hex.EncodeToString(h.Sum(nil))
		return nil
	}

	// The manifest, line by line as its SHA-256 was computed.
	if err := add("manifest.jsonl", func(out io.Writer) error {
		after := int64(0)
		for {
			page, err := s.deps.Datasets.VersionMembers(ctx, p, datasetID, versionID, after, 1000)
			if err != nil {
				return err
			}
			for _, m := range page {
				if _, err := out.Write(m.CanonicalLine()); err != nil {
					return err
				}
				after = m.Ordinal
			}
			if len(page) < 1000 {
				return nil
			}
		}
	}); err != nil {
		return err
	}
	if err := add("signed-manifest.json", func(out io.Writer) error { return writeJSON(out, signed) }); err != nil {
		return err
	}

	prefix := strings.Trim(controlPrefix.ReplaceAllString(strings.ToUpper(ds.Name), "-"), "-")
	if prefix == "" {
		prefix = "DOC"
	}
	control := func(ordinal int64) string { return fmt.Sprintf("%s-%08d", prefix, ordinal) }
	if err := add("custody.jsonl", func(out io.Writer) error {
		for start := 0; start < len(rows); start += 500 {
			batch := rows[start:min(start+500, len(rows))]
			batchIDs := make([]string, len(batch))
			for i, r := range batch {
				batchIDs[i] = r.AssetID
			}
			reads, err := s.deps.Store.Reads(ctx, p.TenantID, batchIDs, opts.Reads)
			if err != nil {
				return err
			}
			for _, r := range batch {
				line := custodyLine{Ordinal: r.Ordinal, ControlNo: control(r.Ordinal), Path: r.Path, AssetID: r.AssetID, ObjectID: r.ObjectID,
					Size: r.Size, ContentType: r.ContentType, SHA256: r.SHA256, MD5: digests[r.AssetID]["md5"], SHA1: digests[r.AssetID]["sha1"],
					UploadedBy: r.UploadedBy, UploadedAt: r.UploadedAt, Connection: r.ConnectionID, StorageVer: r.StorageVersion,
					Tier: r.Tier, Status: r.Status, Reads: reads[r.AssetID]}
				if r.LockMode != "" || r.LegalHold || r.RetainUntil != nil {
					line.Lock = map[string]any{"mode": r.LockMode, "retain_until": r.RetainUntil, "legal_hold": r.LegalHold}
				}
				if line.Reads == nil {
					line.Reads = []any{}
				}
				if err := writeLine(out, line); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// The chain's head now and its latest anchor, with the public key.
	seq, head, err := s.deps.Store.Head(ctx, p.TenantID)
	if err != nil {
		return err
	}
	anchors, err := s.deps.Store.Anchors(ctx, p.TenantID, 1)
	if err != nil {
		return err
	}
	chain := map[string]any{"organization_id": p.TenantID, "head_seq": seq, "head_hash": hex.EncodeToString(head), "read_at": s.now()}
	if len(anchors) > 0 {
		chain["anchor"] = map[string]any{"body": anchors[0].Body, "signature": anchors[0].Signature, "key_id": anchors[0].KeyID,
			"object_key": anchors[0].ObjectKey, "locked_until": anchors[0].LockedUntil}
	}
	if s.deps.Signer.Key != nil {
		chain["public_key"] = base64.StdEncoding.EncodeToString(s.deps.Signer.Key.Public().(ed25519.PublicKey))
		chain["key_id"] = s.deps.Signer.KeyID
	}
	if err := add("audit-chain.json", func(out io.Writer) error { return writeJSON(out, chain) }); err != nil {
		return err
	}

	if err := add("loadfile.dat", func(out io.Writer) error { return writeDAT(out, rows, digests, control) }); err != nil {
		return err
	}
	if err := add("loadfile.opt", func(out io.Writer) error {
		for _, r := range rows {
			if _, err := fmt.Fprintf(out, "%s,VOL001,%s,Y,,,1\r\n", control(r.Ordinal), nativeLink(r.Path)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := add("verify_pack.py", func(out io.Writer) error { _, err := io.WriteString(out, verifyScript); return err }); err != nil {
		return err
	}
	readme := fmt.Sprintf(packReadme, ds.Name, v.Version, v.ID, v.ManifestSHA256, len(rows), s.now().Format(time.RFC3339))
	if err := add("README.md", func(out io.Writer) error { _, err := io.WriteString(out, readme); return err }); err != nil {
		return err
	}

	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	f, err := z.Create("SHA256SUMS")
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := fmt.Fprintf(f, "%s  %s\n", sums[name], name); err != nil {
			return err
		}
	}
	return z.Close()
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeLine(out io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = out.Write(append(raw, '\n'))
	return err
}

// computeDigests reads each member once, through a shard stream (so the
// reads are authorized, audited and checked against SHA-256), to add MD5
// and SHA-1 where they are not known yet.
func (s *Service) computeDigests(ctx context.Context, p model.Principal, rows []postgres.CustodyRow, known map[string]map[string]string) error {
	var todo []string
	for _, r := range rows {
		if r.Status != "AVAILABLE" {
			continue
		}
		if d := known[r.AssetID]; d["md5"] == "" || d["sha1"] == "" {
			todo = append(todo, r.AssetID)
		}
	}
	for start := 0; start < len(todo); start += 1000 {
		batch := todo[start:min(start+1000, len(todo))]
		err := s.deps.Delivery.StreamShard(ctx, p, batch, "evidence-pack", func(f delivery.ShardFile) error {
			hm, hs := md5.New(), sha1.New()
			if _, err := io.Copy(io.MultiWriter(hm, hs), f.Body); err != nil {
				return err
			}
			d := map[string]string{"md5": hexOf(hm), "sha1": hexOf(hs)}
			if known[f.AssetID] == nil {
				known[f.AssetID] = map[string]string{}
			}
			for k, v := range d {
				known[f.AssetID][k] = v
			}
			return s.deps.Store.PutDigests(ctx, p.TenantID, f.AssetID, d, s.now())
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func hexOf(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// Concordance load files: fields between þ, separated by ¶ (0x14).
const (
	datQuote = "þ"
	datSep   = "\x14"
)

func nativeLink(path string) string {
	return `NATIVES\` + strings.ReplaceAll(path, "/", `\`)
}

func writeDAT(out io.Writer, rows []postgres.CustodyRow, digests map[string]map[string]string, control func(int64) string) error {
	if _, err := io.WriteString(out, "\ufeff"); err != nil {
		return err
	}
	line := func(fields ...string) error {
		for i, f := range fields {
			fields[i] = datQuote + strings.ReplaceAll(f, datQuote, "") + datQuote
		}
		_, err := io.WriteString(out, strings.Join(fields, datSep)+"\r\n")
		return err
	}
	if err := line("BEGDOC", "ENDDOC", "FILENAME", "FILEPATH", "FILESIZE", "MD5HASH", "SHA1HASH", "SHA256HASH", "CUSTODIAN",
		"DATECREATED", "NATIVELINK"); err != nil {
		return err
	}
	for _, r := range rows {
		name := r.Path
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		created := ""
		if r.UploadedAt != nil {
			created = r.UploadedAt.UTC().Format("01/02/2006 15:04:05")
		}
		c := control(r.Ordinal)
		if err := line(c, c, name, r.Path, fmt.Sprint(r.Size), digests[r.AssetID]["md5"], digests[r.AssetID]["sha1"], r.SHA256,
			r.UploadedBy, created, nativeLink(r.Path)); err != nil {
			return err
		}
	}
	return nil
}

const packReadme = `# Evidence pack: %s version %d

Snapshot %s, manifest SHA-256 %s, %d members. Made %s.

| File | What it holds |
|---|---|
| manifest.jsonl | The snapshot's canonical manifest, one line per member in ordinal order. Its SHA-256 is the snapshot's manifest hash. |
| signed-manifest.json | The snapshot's identity (manifest hash, counts), signed Ed25519 by the platform. |
| custody.jsonl | Each member's chain of custody: SHA-256 (and MD5, SHA-1 when computed), who uploaded it and when, where it is stored, its Object Lock, and its reads. |
| audit-chain.json | The organization's audit chain head when this pack was made, its latest anchor (signed and kept outside the database), and the public key. |
| loadfile.dat, loadfile.opt | Concordance load files (fields in þ, separated by 0x14; control numbers per member) for review tools; natives are expected under NATIVES\. |
| verify_pack.py | Checks this pack without the platform: python verify_pack.py [--natives FOLDER]. |
| SHA256SUMS | The SHA-256 of every file above. |

To check: run python verify_pack.py in this folder. It checks SHA256SUMS, that manifest.jsonl hashes to the signed manifest hash, the signatures (with the cryptography package installed), and, given the natives (osp download), every file's SHA-256, MD5 and SHA-1.
`

const verifyScript = `"""Checks an evidence pack without the platform (Python 3, standard library;
signatures need the cryptography package).

    python verify_pack.py [--natives FOLDER]
"""
import base64, hashlib, json, os, sys

ok = True
def say(good, text):
    global ok
    ok = ok and good
    print(("ok    " if good else "FAIL  ") + text)

here = os.path.dirname(os.path.abspath(__file__))
def path(name): return os.path.join(here, name)
def sha256(name, h=hashlib.sha256):
    d = h()
    with open(name, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            d.update(block)
    return d.hexdigest()

for line in open(path("SHA256SUMS"), encoding="utf-8"):
    want, name = line.strip().split("  ", 1)
    say(os.path.exists(path(name)) and sha256(path(name)) == want, "SHA256SUMS " + name)

signed = json.load(open(path("signed-manifest.json"), encoding="utf-8"))
body = json.loads(signed["body"])
say(sha256(path("manifest.jsonl")) == body["manifest_sha256"], "manifest.jsonl hashes to the signed manifest hash " + body["manifest_sha256"])

chain = json.load(open(path("audit-chain.json"), encoding="utf-8"))
try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    key = Ed25519PublicKey.from_public_bytes(base64.b64decode(chain["public_key"]))
    def check(text, sig, what):
        try:
            key.verify(base64.b64decode(sig), text.encode("utf-8"))
            say(True, what + " signature")
        except Exception:
            say(False, what + " signature")
    if signed.get("signature"):
        check(signed["body"], signed["signature"], "manifest")
    if chain.get("anchor", {}).get("signature"):
        check(chain["anchor"]["body"], chain["anchor"]["signature"], "audit anchor")
except (ImportError, KeyError):
    print("skip  signatures: install cryptography, or the platform signs nothing")

if "--natives" in sys.argv:
    folder = sys.argv[sys.argv.index("--natives") + 1]
    for line in open(path("custody.jsonl"), encoding="utf-8"):
        m = json.loads(line)
        f = os.path.join(folder, *m["path"].split("/"))
        if not os.path.exists(f):
            say(m["status"] != "AVAILABLE", "missing " + m["path"])
            continue
        good = sha256(f) == m["sha256"]
        if m.get("md5"):
            good = good and sha256(f, hashlib.md5) == m["md5"]
        if m.get("sha1"):
            good = good and sha256(f, hashlib.sha1) == m["sha1"]
        say(good, m["path"])

print("the pack checks out" if ok else "the pack does NOT check out")
sys.exit(0 if ok else 1)
`
