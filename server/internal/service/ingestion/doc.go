// Package ingestion runs bulk ingestion: datasets, manifest-driven jobs,
// upload-slot scheduling, entry completion and outcomes, retries,
// cancellation and reconciliation. Clients upload file bytes straight to
// object storage; this package only decides who may upload what, when,
// and records what happened. Entry paths follow the catalog's rules
// (model.NormalizePath): each published entry becomes a catalog object.
package ingestion
