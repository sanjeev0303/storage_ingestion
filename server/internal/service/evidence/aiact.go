package evidence

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// TrainingSummary is a model's training-content summary, laid out as the
// European Commission's template for general-purpose AI models (July
// 2025): general information, the list of data sources by kind, and the
// data processing aspects. What the platform cannot know is marked for
// the provider to complete.
type TrainingSummary struct {
	Template   string                   `json:"template"`
	Model      SummaryModel             `json:"model"`
	Modalities []Modality               `json:"modalities"`
	Sources    map[string][]SourceEntry `json:"sources"`
	Processing []string                 `json:"processing"`
	ToComplete []string                 `json:"to_complete"`
	Published  *time.Time               `json:"published_at,omitempty"`
	NextDue    time.Time                `json:"next_due"`
}

type SummaryModel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	URI       string    `json:"uri,omitempty"`
	RunID     string    `json:"run_id"`
	RunName   string    `json:"run_name"`
	TrainedAt time.Time `json:"trained_at"`
}

type Modality struct {
	Kind  string `json:"kind"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
}

type SourceEntry struct {
	Dataset        string           `json:"dataset"`
	Version        int              `json:"version"`
	LawfulBasis    string           `json:"lawful_basis,omitempty"`
	License        string           `json:"license,omitempty"`
	Source         string           `json:"source,omitempty"`
	Files          int64            `json:"files"`
	Bytes          int64            `json:"bytes"`
	From           *time.Time       `json:"collected_from,omitempty"`
	To             *time.Time       `json:"collected_to,omitempty"`
	MemberLicenses map[string]int64 `json:"member_licenses,omitempty"`
	MemberSources  map[string]int64 `json:"member_sources,omitempty"`
}

var sourceSections = map[string]string{
	"PUBLIC_DATASET": "2.1 Publicly available datasets", "LICENSED_PRIVATE": "2.2 Private datasets obtained from third parties",
	"WEB_CRAWLED": "2.3 Data crawled and scraped from online sources", "USER_DATA": "2.4 User data",
	"SYNTHETIC": "2.5 Synthetic data", "OTHER": "2.6 Other sources", "": "2.6 Other sources (kind not recorded)",
}

// ModelSummary walks a model's lineage (the run, the snapshots it read,
// their datasets) and fills the template.
func (s *Service) ModelSummary(ctx context.Context, p model.Principal, modelID string) (TrainingSummary, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "model.training_summary", "model", modelID); err != nil {
		return TrainingSummary{}, err
	}
	m, err := s.deps.Store.Model(ctx, p.TenantID, modelID)
	if err != nil {
		return TrainingSummary{}, err
	}
	out := TrainingSummary{Template: "EU AI Act, Article 53(1)(d): public summary of training content (Commission template, July 2025)",
		Model:   SummaryModel{ID: m.ID, Name: m.Name, Version: m.Version, URI: m.URI, RunID: m.RunID, RunName: m.RunName, TrainedAt: m.RunCreatedAt},
		Sources: map[string][]SourceEntry{}, Published: m.SummaryPublishedAt}
	base := m.CreatedAt
	if m.SummaryPublishedAt != nil {
		base = *m.SummaryPublishedAt
	}
	out.NextDue = base.AddDate(0, 6, 0)

	orders, err := s.deps.Store.VersionOrders(ctx, p.TenantID, m.VersionIDs)
	if err != nil {
		return TrainingSummary{}, err
	}
	modal := map[string]Modality{}
	for _, o := range orders {
		prof, err := s.deps.Store.VersionProfile(ctx, p.TenantID, o.VersionID)
		if err != nil {
			return TrainingSummary{}, err
		}
		e := SourceEntry{Dataset: o.DatasetName, Version: o.Version, LawfulBasis: o.Provenance.LawfulBasis, License: o.Provenance.License,
			Source: o.Provenance.Source, From: prof.First, To: prof.Last, MemberLicenses: prof.Licenses, MemberSources: prof.Sources}
		for kind, nb := range prof.Modalities {
			e.Files += nb[0]
			e.Bytes += nb[1]
			x := modal[kind]
			x.Kind, x.Files, x.Bytes = kind, x.Files+nb[0], x.Bytes+nb[1]
			modal[kind] = x
		}
		section := sourceSections[o.Provenance.SourceKind]
		out.Sources[section] = append(out.Sources[section], e)
		if o.Provenance.SourceKind == "" {
			out.ToComplete = append(out.ToComplete, fmt.Sprintf("record the kind of source of %s (its provenance)", o.DatasetName))
		}
	}
	for _, x := range modal {
		out.Modalities = append(out.Modalities, x)
	}
	sort.Slice(out.Modalities, func(i, j int) bool { return out.Modalities[i].Bytes > out.Modalities[j].Bytes })
	out.Processing = []string{
		"Every file was checked against its SHA-256 at upload and scanned for malware before it could be read.",
		"Each snapshot pins exact object versions; this summary is generated from the snapshots the run recorded reading.",
	}
	out.ToComplete = append(out.ToComplete,
		"1.1 the provider's name and contact point",
		"1.2 the date the model was placed on the market",
		"3.1 how reservations of rights (text and data mining opt-outs) were respected",
		"3.2 measures taken to remove illegal content")
	return out, nil
}

// Markdown renders a training summary for publication.
func (t TrainingSummary) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Public summary of training content: %s %s\n\n_%s_\n\n", t.Model.Name, t.Model.Version, t.Template)
	fmt.Fprintf(&b, "## 1. General information\n\n- Model: %s, version %s (%s)\n- Trained in run %s (%s), recorded %s\n- Provider and placing on the market: to be completed by the provider\n\n",
		t.Model.Name, t.Model.Version, t.Model.ID, t.Model.RunName, t.Model.RunID, t.Model.TrainedAt.Format("2006-01-02"))
	b.WriteString("### 1.3 Modalities and size of the training data\n\n| Modality | Files | Size |\n|---|---|---|\n")
	for _, m := range t.Modalities {
		fmt.Fprintf(&b, "| %s | %d | %s |\n", m.Kind, m.Files, sizeText(m.Bytes))
	}
	b.WriteString("\n## 2. List of data sources\n\n")
	sections := make([]string, 0, len(t.Sources))
	for s := range t.Sources {
		sections = append(sections, s)
	}
	sort.Strings(sections)
	for _, sec := range sections {
		fmt.Fprintf(&b, "### %s\n\n", sec)
		for _, e := range t.Sources[sec] {
			fmt.Fprintf(&b, "- **%s** (version %d): %d files, %s", e.Dataset, e.Version, e.Files, sizeText(e.Bytes))
			if e.From != nil && e.To != nil {
				fmt.Fprintf(&b, ", collected %s to %s", e.From.Format("2006-01-02"), e.To.Format("2006-01-02"))
			}
			fmt.Fprintf(&b, ". Source: %s. Licence: %s. Lawful basis: %s.\n", orNot(e.Source), orNot(e.License), orNot(e.LawfulBasis))
		}
		b.WriteString("\n")
	}
	b.WriteString("## 3. Data processing aspects\n\n")
	for _, x := range t.Processing {
		fmt.Fprintf(&b, "- %s\n", x)
	}
	if len(t.ToComplete) > 0 {
		b.WriteString("\n## To complete before publishing\n\n")
		for _, x := range t.ToComplete {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	fmt.Fprintf(&b, "\nNext update due by %s (every six months).\n", t.NextDue.Format("2006-01-02"))
	return b.String()
}

func orNot(s string) string {
	if s == "" {
		return "not recorded"
	}
	return s
}

func sizeText(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// PublishSummary records that a model's summary was published, which
// starts its six months.
func (s *Service) PublishSummary(ctx context.Context, p model.Principal, modelID string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "model.summary_published", "model", modelID); err != nil {
		return err
	}
	if err := s.deps.Store.PublishSummary(ctx, p.TenantID, modelID, s.now()); err != nil {
		return err
	}
	s.record(ctx, p, "model.summary_published", "model", modelID, nil)
	return nil
}

// RemindSummaries sends model.summary_due for summaries six months old.
func (s *Service) RemindSummaries(ctx context.Context) (int, error) {
	return s.deps.Store.SummariesDue(ctx, s.now())
}
