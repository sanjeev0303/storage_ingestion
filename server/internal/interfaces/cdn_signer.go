package interfaces

import "time"

// URLSigner signs a CDN URL so the CDN serves it only until expires.
// This is the CDN's own authorization, separate from storage presigning:
// the CDN checks the signature and reaches the private origin with its own
// credentials.
type URLSigner interface {
	SignURL(rawURL string, expires time.Time) (string, error)
}
