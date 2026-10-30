package processing

import (
	"fmt"
	"io"
	"regexp"
)

// secretPatterns are credentials with a recognisable shape. A finding
// names the kind and where it is, never the value: the platform must not
// copy a leaked secret into its own records.
var secretPatterns = []struct {
	code string
	what string
	re   *regexp.Regexp
}{
	{"secret.aws_access_key", "an AWS access key ID", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"secret.aws_secret_key", "an AWS secret access key", regexp.MustCompile(`(?i)aws_?secret_?(?:access_?)?key["']?\s*[:=]\s*["']?[A-Za-z0-9/+]{40}\b`)},
	{"secret.private_key", "a private key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY(?: BLOCK)?-----`)},
	{"secret.github_token", "a GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{82})\b`)},
	{"secret.slack_token", "a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"secret.stripe_key", "a live Stripe key", regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{24,}`)},
	{"secret.google_api_key", "a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"secret.azure_storage_key", "an Azure storage account key", regexp.MustCompile(`AccountKey=[A-Za-z0-9+/]{86}==`)},
	{"secret.ai_api_key", "an AI provider API key", regexp.MustCompile(`\bsk-(?:ant-api\d\d|proj)-[A-Za-z0-9_\-]{20,}`)},
	{"secret.jwt", "a signed JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{20,}`)},
}

// scanSecrets reads up to limit bytes in chunks that overlap by more than
// the longest match, so a secret split between two reads is still found.
func scanSecrets(r io.Reader, limit int64, in *inspection) error {
	if limit <= 0 {
		limit = 64 << 20
	}
	const chunk, overlap = 1 << 20, 512
	type hit struct {
		at    int64
		count int
	}
	hits := map[string]*hit{}
	buf := make([]byte, 0, chunk+overlap)
	read := make([]byte, chunk)
	var base, total int64 // base: offset of buf[0]
	for {
		n, err := io.ReadFull(r, read)
		total += int64(n)
		buf = append(buf, read[:n]...)
		last := err != nil || total >= limit
		// Matches starting in the overlap are counted with the next chunk.
		edge := len(buf) - overlap
		if last {
			edge = len(buf)
		}
		for _, p := range secretPatterns {
			for _, m := range p.re.FindAllIndex(buf, -1) {
				if m[0] >= edge {
					continue
				}
				h := hits[p.code]
				if h == nil {
					h = &hit{at: base + int64(m[0])}
					hits[p.code] = h
				}
				h.count++
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		if total >= limit {
			in.note("secret scan read the first %d bytes; the rest were not scanned", limit)
			break
		}
		keep := min(overlap, len(buf))
		base += int64(len(buf) - keep)
		buf = append(buf[:0], buf[len(buf)-keep:]...)
	}
	for _, p := range secretPatterns {
		if h := hits[p.code]; h != nil {
			in.find("secrets", p.code, fmt.Sprintf("%s at byte %d (%d found)", p.what, h.at, h.count))
		}
	}
	return nil
}
