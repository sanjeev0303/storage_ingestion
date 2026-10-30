package dataset

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// The rules engine. A profile's rules are compiled once per job pass into
// checks: member checks look at one version member and what is known about
// it, dataset checks at the version's aggregates. Checks are pure: the
// same inputs give the same issues, which is what makes findings stable
// when a batch is evaluated again.

// Readiness dimensions a rule speaks to.
const (
	DimCompleteness    = "COMPLETENESS"
	DimIntegrity       = "INTEGRITY"
	DimSecurity        = "SECURITY"
	DimConsistency     = "CONSISTENCY"
	DimDuplication     = "DUPLICATION"
	DimDistribution    = "DISTRIBUTION"
	DimReproducibility = "REPRODUCIBILITY"
	DimGovernance      = "GOVERNANCE"
	DimAccessibility   = "ACCESSIBILITY"
)

// Dimensions in report order.
var Dimensions = []string{DimCompleteness, DimIntegrity, DimSecurity, DimConsistency, DimDuplication,
	DimDistribution, DimReproducibility, DimGovernance, DimAccessibility}

// issue is one problem a check found, before it becomes a finding.
type issue struct {
	Code        string
	Message     string
	Expected    string
	Observed    string
	Remediation string
	// Key tells apart issues of one rule with the same code: the member's
	// object, or a dataset-level group.
	Key      string
	ObjectID string
	AssetID  string
}

// check is a compiled rule.
type check struct {
	rule      model.Rule
	dimension string
	// Exactly one of member, content and dataset is set. content checks
	// read object bytes (or trusted evidence of them) and run only in FULL
	// and SAMPLED modes.
	member func(m model.MemberEvidence) []issue
	// applies, when set, says whether member says anything about m; a
	// member it does not apply to counts as not assessed, not as passed.
	applies func(m model.MemberEvidence) bool
	content *contentRule
	dataset func(a model.VersionAggregates) ([]issue, string)
	// load, when set, reads what a dataset check needs beyond the
	// aggregates and returns the check to run (rules_quality.go).
	load func(ctx context.Context, s *Service, tenantID, versionID string) func(model.VersionAggregates) ([]issue, string)
}

// contentRule configures the content integrity check.
type contentRule struct {
	// Reverify reads the bytes even when processing already verified
	// them.
	Reverify bool `json:"reverify"`
}

// ruleType describes one rule type: its scope, the dimension it informs,
// and how to compile its configuration.
type ruleType struct {
	scope     model.RuleScope
	dimension string
	compile   func(c *check, config json.RawMessage) error
}

var ruleTypes = map[string]ruleType{
	// Structural, per member.
	"required_metadata":   {model.RuleScopeObjectMetadata, DimCompleteness, compileRequiredMetadata},
	"governance_metadata": {model.RuleScopeObjectMetadata, DimGovernance, compileRequiredMetadata},
	"metadata_schema":     {model.RuleScopeObjectMetadata, DimConsistency, compileMetadataSchema},
	"allowed_media_types": {model.RuleScopeObjectMetadata, DimConsistency, compileMediaTypes},
	"allowed_extensions":  {model.RuleScopeObjectMetadata, DimConsistency, compileExtensions},
	"object_size":         {model.RuleScopeObjectMetadata, DimConsistency, compileObjectSize},
	"path_pattern":        {model.RuleScopeObjectMetadata, DimConsistency, compilePathPattern},
	"required_label":      {model.RuleScopeObjectMetadata, DimCompleteness, compileRequiredLabel},
	"split_assignment":    {model.RuleScopeSplit, DimConsistency, compileSplitAssignment},
	// Integrity, per member, from catalog records and processing evidence.
	"object_exists":     {model.RuleScopeObjectMetadata, DimCompleteness, compileObjectExists},
	"lifecycle_state":   {model.RuleScopeObjectMetadata, DimAccessibility, compileLifecycleState},
	"size_consistency":  {model.RuleScopeObjectMetadata, DimIntegrity, compileSizeConsistency},
	"checksum_present":  {model.RuleScopeObjectMetadata, DimIntegrity, compileChecksumPresent},
	"checksum_verified": {model.RuleScopeObjectMetadata, DimIntegrity, compileChecksumVerified},
	"malware_scan":      {model.RuleScopeObjectMetadata, DimSecurity, compileMalwareScan},
	"content_checks":    {model.RuleScopeObjectMetadata, DimSecurity, compileContentChecks},
	"storage_integrity": {model.RuleScopeObjectMetadata, DimIntegrity, compileStorageIntegrity},
	"residency":         {model.RuleScopeObjectMetadata, DimGovernance, compileResidency},
	// Content, per member: bytes or trusted evidence of them.
	"content_checksum": {model.RuleScopeObjectContent, DimIntegrity, compileContentChecksum},
	// Dataset level, from aggregates.
	"member_count":           {model.RuleScopeDataset, DimCompleteness, compileMemberCount},
	"total_bytes":            {model.RuleScopeDataset, DimDistribution, compileTotalBytes},
	"duplicate_references":   {model.RuleScopeDataset, DimDuplication, compileDuplicateReferences},
	"duplicate_content":      {model.RuleScopeDataset, DimDuplication, compileDuplicateContent},
	"split_leakage":          {model.RuleScopeSplit, DimConsistency, compileSplitLeakage},
	"split_proportions":      {model.RuleScopeSplit, DimDistribution, compileSplitProportions},
	"required_labels":        {model.RuleScopeDataset, DimCompleteness, compileRequiredLabels},
	"label_distribution":     {model.RuleScopeDataset, DimDistribution, compileLabelDistribution},
	"file_type_distribution": {model.RuleScopeDataset, DimDistribution, compileFileTypeDistribution},
	"small_files":            {model.RuleScopeDataset, DimAccessibility, compileSmallFiles},
	// Dataset level, from the column profile and member metadata.
	"column_drift":     {model.RuleScopeDataset, DimDistribution, compileColumnDrift},
	"uniform_metadata": {model.RuleScopeDataset, DimConsistency, compileUniformMetadata},
	"subject_leakage":  {model.RuleScopeSplit, DimConsistency, compileSubjectLeakage},
}

// RuleTypes lists the rule types profiles may use.
func RuleTypes() []string {
	names := make([]string, 0, len(ruleTypes))
	for name := range ruleTypes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// compile turns a profile's enabled rules into checks, refusing anything
// malformed: unknown types or fields, bad values, repeated IDs.
func compile(def model.ProfileDefinition) ([]check, error) {
	if len(def.Rules) == 0 || len(def.Rules) > 100 {
		return nil, fmt.Errorf("%w: a profile has 1 to 100 rules", model.ErrInvalidProfile)
	}
	if def.Thresholds.MaxErrors < 0 || (def.Thresholds.MaxWarnings != nil && *def.Thresholds.MaxWarnings < 0) {
		return nil, fmt.Errorf("%w: thresholds must not be negative", model.ErrInvalidProfile)
	}
	seen := map[string]bool{}
	var checks []check
	for _, r := range def.Rules {
		if !ruleIDPattern.MatchString(r.ID) {
			return nil, fmt.Errorf("%w: rule ID %q: 1-64 characters of a-z, 0-9, '.', '_' and '-'", model.ErrInvalidProfile, r.ID)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("%w: rule ID %q appears twice", model.ErrInvalidProfile, r.ID)
		}
		seen[r.ID] = true
		t, ok := ruleTypes[r.Type]
		if !ok {
			return nil, fmt.Errorf("%w: rule %s: unknown type %q", model.ErrInvalidProfile, r.ID, r.Type)
		}
		if r.Version < 1 || !r.Severity.Valid() {
			return nil, fmt.Errorf("%w: rule %s: version must be positive and severity ERROR, WARNING or INFO", model.ErrInvalidProfile, r.ID)
		}
		if r.Scope == "" {
			r.Scope = t.scope
		}
		if r.Scope != t.scope {
			return nil, fmt.Errorf("%w: rule %s: a %s rule has scope %s", model.ErrInvalidProfile, r.ID, r.Type, t.scope)
		}
		if !r.Enabled {
			continue
		}
		c := check{rule: r, dimension: t.dimension}
		if err := t.compile(&c, r.Config); err != nil {
			return nil, fmt.Errorf("%w: rule %s: %v", model.ErrInvalidProfile, r.ID, err)
		}
		checks = append(checks, c)
	}
	return checks, nil
}

// decodeConfig reads a rule's configuration strictly: unknown fields are
// an error, so a typo cannot silently turn a check off.
func decodeConfig(config json.RawMessage, into any) error {
	if len(config) == 0 || string(config) == "null" {
		config = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("config: %v", err)
	}
	return nil
}

func memberIssue(m model.MemberEvidence, code, message string) issue {
	return issue{Code: code, Message: message, Key: m.ObjectID, ObjectID: m.ObjectID, AssetID: m.AssetID}
}

// ─── Structural ───────────────────────────────────────────────────────

func compileRequiredMetadata(c *check, config json.RawMessage) error {
	var cfg struct {
		Keys []string `json:"keys"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Keys) == 0 || len(cfg.Keys) > 64 {
		return fmt.Errorf("keys: 1 to 64 metadata keys")
	}
	for _, k := range cfg.Keys {
		if !model.ValidMetadataKey(k) {
			return fmt.Errorf("keys: %q is not a metadata key", k)
		}
	}
	code := "MISSING_METADATA"
	if c.rule.Type == "governance_metadata" {
		code = "MISSING_GOVERNANCE_METADATA"
	}
	c.member = func(m model.MemberEvidence) []issue {
		var missing []string
		for _, k := range cfg.Keys {
			if strings.TrimSpace(m.Metadata[k]) == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		is := memberIssue(m, code, "required metadata is missing")
		is.Expected = strings.Join(cfg.Keys, ",")
		is.Observed = "missing: " + strings.Join(missing, ",")
		is.Remediation = "set the keys on the member or on the object version, then take a new snapshot"
		return []issue{is}
	}
	return nil
}

// metadataField types a metadata value; values are strings in the
// catalog, so a type is what the string must parse as.
type metadataField struct {
	Type    string   `json:"type"`
	Enum    []string `json:"enum"`
	Pattern string   `json:"pattern"`
}

func compileMetadataSchema(c *check, config json.RawMessage) error {
	var cfg struct {
		Fields       map[string]metadataField `json:"fields"`
		AllowUnknown bool                     `json:"allow_unknown"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Fields) == 0 || len(cfg.Fields) > 64 {
		return fmt.Errorf("fields: 1 to 64 metadata keys")
	}
	patterns := map[string]*regexp.Regexp{}
	for k, f := range cfg.Fields {
		if !model.ValidMetadataKey(k) {
			return fmt.Errorf("fields: %q is not a metadata key", k)
		}
		switch f.Type {
		case "", "string", "integer", "number", "boolean":
		default:
			return fmt.Errorf("fields.%s.type: string, integer, number or boolean", k)
		}
		if f.Pattern != "" {
			if len(f.Pattern) > 256 {
				return fmt.Errorf("fields.%s.pattern: at most 256 characters", k)
			}
			re, err := regexp.Compile(f.Pattern)
			if err != nil {
				return fmt.Errorf("fields.%s.pattern: %v", k, err)
			}
			patterns[k] = re
		}
	}
	keys := make([]string, 0, len(cfg.Fields))
	for k := range cfg.Fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	c.member = func(m model.MemberEvidence) []issue {
		var problems []string
		for _, k := range keys {
			v, ok := m.Metadata[k]
			if !ok {
				continue // required_metadata says whether a key must be there
			}
			f := cfg.Fields[k]
			if !typed(v, f.Type) {
				problems = append(problems, k+" is not "+f.Type)
			} else if len(f.Enum) > 0 && !slices.Contains(f.Enum, v) {
				problems = append(problems, k+" is not an allowed value")
			} else if re := patterns[k]; re != nil && !re.MatchString(v) {
				problems = append(problems, k+" does not match its pattern")
			}
		}
		if !cfg.AllowUnknown {
			var unknown []string
			for k := range m.Metadata {
				if _, ok := cfg.Fields[k]; !ok {
					unknown = append(unknown, k)
				}
			}
			slices.Sort(unknown)
			if len(unknown) > 0 {
				problems = append(problems, "unexpected keys: "+strings.Join(unknown, ","))
			}
		}
		if len(problems) == 0 {
			return nil
		}
		// Values are not echoed: metadata may be sensitive.
		is := memberIssue(m, "METADATA_SCHEMA", "metadata does not match the schema")
		is.Observed = strings.Join(problems, "; ")
		is.Remediation = "correct the member's metadata or the profile's schema"
		return []issue{is}
	}
	return nil
}

func typed(v, t string) bool {
	switch t {
	case "integer":
		_, err := strconv.ParseInt(v, 10, 64)
		return err == nil
	case "number":
		f, err := strconv.ParseFloat(v, 64)
		return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case "boolean":
		return v == "true" || v == "false"
	}
	return true
}

func compileMediaTypes(c *check, config json.RawMessage) error {
	var cfg struct {
		Types []string `json:"types"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Types) == 0 || len(cfg.Types) > 100 {
		return fmt.Errorf("types: 1 to 100 media types, such as image/png or image/*")
	}
	c.member = func(m model.MemberEvidence) []issue {
		if mediaTypeAllowed(m.ContentType, cfg.Types) {
			return nil
		}
		is := memberIssue(m, "MEDIA_TYPE_NOT_ALLOWED", "the media type is not allowed by the profile")
		is.Expected, is.Observed = strings.Join(cfg.Types, ","), m.ContentType
		is.Remediation = "remove the member, or allow its type in the profile"
		return []issue{is}
	}
	return nil
}

func mediaTypeAllowed(contentType string, allowed []string) bool {
	base, _, _ := strings.Cut(strings.ToLower(contentType), ";")
	base = strings.TrimSpace(base)
	for _, a := range allowed {
		a = strings.ToLower(a)
		if a == base || (strings.HasSuffix(a, "/*") && strings.HasPrefix(base, strings.TrimSuffix(a, "*"))) {
			return true
		}
	}
	return false
}

// extensionOf is a path's lowercase extension without the dot.
func extensionOf(p string) string {
	return strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")
}

func compileExtensions(c *check, config json.RawMessage) error {
	var cfg struct {
		Extensions []string `json:"extensions"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Extensions) == 0 || len(cfg.Extensions) > 100 {
		return fmt.Errorf("extensions: 1 to 100 extensions, without the dot")
	}
	allowed := make([]string, len(cfg.Extensions))
	for i, e := range cfg.Extensions {
		allowed[i] = strings.TrimPrefix(strings.ToLower(e), ".")
	}
	c.member = func(m model.MemberEvidence) []issue {
		ext := extensionOf(m.Path)
		if slices.Contains(allowed, ext) {
			return nil
		}
		is := memberIssue(m, "EXTENSION_NOT_ALLOWED", "the file extension is not allowed by the profile")
		is.Expected, is.Observed = strings.Join(allowed, ","), ext
		is.Remediation = "rename the object, remove the member, or allow the extension"
		return []issue{is}
	}
	return nil
}

func compileObjectSize(c *check, config json.RawMessage) error {
	var cfg struct {
		MinBytes int64 `json:"min_bytes"`
		MaxBytes int64 `json:"max_bytes"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if cfg.MinBytes < 0 || cfg.MaxBytes < 0 || (cfg.MaxBytes > 0 && cfg.MaxBytes < cfg.MinBytes) || (cfg.MinBytes == 0 && cfg.MaxBytes == 0) {
		return fmt.Errorf("min_bytes and max_bytes: a non-negative range, at least one of them set")
	}
	c.member = func(m model.MemberEvidence) []issue {
		switch {
		case m.SizeBytes < cfg.MinBytes:
			is := memberIssue(m, "OBJECT_TOO_SMALL", "the object is smaller than the profile allows")
			is.Expected, is.Observed = fmt.Sprintf(">= %d bytes", cfg.MinBytes), fmt.Sprintf("%d bytes", m.SizeBytes)
			return []issue{is}
		case cfg.MaxBytes > 0 && m.SizeBytes > cfg.MaxBytes:
			is := memberIssue(m, "OBJECT_TOO_LARGE", "the object is larger than the profile allows")
			is.Expected, is.Observed = fmt.Sprintf("<= %d bytes", cfg.MaxBytes), fmt.Sprintf("%d bytes", m.SizeBytes)
			return []issue{is}
		}
		return nil
	}
	return nil
}

func compilePathPattern(c *check, config json.RawMessage) error {
	var cfg struct {
		Pattern string `json:"pattern"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if cfg.Pattern == "" || len(cfg.Pattern) > 256 {
		return fmt.Errorf("pattern: a regular expression of at most 256 characters")
	}
	// RE2: matching time is linear in the path, whatever the pattern.
	re, err := regexp.Compile(cfg.Pattern)
	if err != nil {
		return fmt.Errorf("pattern: %v", err)
	}
	c.member = func(m model.MemberEvidence) []issue {
		if re.MatchString(m.Path) {
			return nil
		}
		is := memberIssue(m, "PATH_PATTERN_MISMATCH", "the object's path does not match the expected pattern")
		is.Expected = cfg.Pattern
		is.Remediation = "rename the object or remove the member"
		return []issue{is}
	}
	return nil
}

func compileRequiredLabel(c *check, config json.RawMessage) error {
	if err := decodeConfig(config, &struct{}{}); err != nil {
		return err
	}
	c.member = func(m model.MemberEvidence) []issue {
		if strings.TrimSpace(m.Label) != "" {
			return nil
		}
		is := memberIssue(m, "MISSING_LABEL", "the member has no label")
		is.Remediation = "label the member in the draft and take a new snapshot"
		return []issue{is}
	}
	return nil
}

func compileSplitAssignment(c *check, config json.RawMessage) error {
	var cfg struct {
		Allowed []string `json:"allowed"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Allowed) == 0 {
		cfg.Allowed = model.DatasetSplits
	}
	for _, s := range cfg.Allowed {
		if !slices.Contains(model.DatasetSplits, s) {
			return fmt.Errorf("allowed: %q is not a split (train, validation, test)", s)
		}
	}
	c.member = func(m model.MemberEvidence) []issue {
		if m.Split == "" {
			is := memberIssue(m, "MISSING_SPLIT", "the member is not assigned to a split")
			is.Expected = strings.Join(cfg.Allowed, ",")
			is.Remediation = "assign the member to a split"
			return []issue{is}
		}
		if !slices.Contains(cfg.Allowed, m.Split) {
			is := memberIssue(m, "SPLIT_NOT_ALLOWED", "the member's split is not allowed by the profile")
			is.Expected, is.Observed = strings.Join(cfg.Allowed, ","), m.Split
			return []issue{is}
		}
		return nil
	}
	return nil
}

// ─── Integrity from records and evidence ──────────────────────────────

func compileObjectExists(c *check, config json.RawMessage) error {
	if err := decodeConfig(config, &struct{}{}); err != nil {
		return err
	}
	c.member = func(m model.MemberEvidence) []issue {
		switch {
		case !m.ObjectExists:
			return []issue{memberIssue(m, "OBJECT_MISSING", "the catalog object no longer exists")}
		case m.AssetStatus == "":
			return []issue{memberIssue(m, "VERSION_MISSING", "the object version no longer exists")}
		case m.ObjectDeleted:
			is := memberIssue(m, "OBJECT_DELETED", "the catalog object was deleted")
			is.Remediation = "restore the object or remove the member"
			return []issue{is}
		}
		return nil
	}
	return nil
}

func compileLifecycleState(c *check, config json.RawMessage) error {
	var cfg struct {
		Allowed []string `json:"allowed"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Allowed) == 0 {
		cfg.Allowed = []string{string(model.AssetStatusAvailable)}
	}
	c.member = func(m model.MemberEvidence) []issue {
		if m.AssetStatus == "" || slices.Contains(cfg.Allowed, string(m.AssetStatus)) {
			return nil // a missing version is object_exists's finding
		}
		is := memberIssue(m, "VERSION_NOT_ACCESSIBLE", "the object version is not in an allowed state")
		is.Expected, is.Observed = strings.Join(cfg.Allowed, ","), string(m.AssetStatus)
		is.Remediation = "restore or replace the version, then take a new snapshot"
		return []issue{is}
	}
	return nil
}

func compileSizeConsistency(c *check, config json.RawMessage) error {
	if err := decodeConfig(config, &struct{}{}); err != nil {
		return err
	}
	c.member = func(m model.MemberEvidence) []issue {
		if m.AssetStatus == "" {
			return nil
		}
		var out []issue
		if m.SizeBytes != m.AssetSizeBytes {
			is := memberIssue(m, "SIZE_INCONSISTENT", "the snapshot's size differs from the version's record")
			is.Expected, is.Observed = strconv.FormatInt(m.SizeBytes, 10), strconv.FormatInt(m.AssetSizeBytes, 10)
			out = append(out, is)
		}
		if m.ChecksumSHA256 != m.AssetChecksum {
			is := memberIssue(m, "CHECKSUM_INCONSISTENT", "the snapshot's checksum differs from the version's record")
			is.Key = m.ObjectID + "/checksum"
			out = append(out, is)
		}
		return out
	}
	return nil
}

func compileChecksumPresent(c *check, config json.RawMessage) error {
	if err := decodeConfig(config, &struct{}{}); err != nil {
		return err
	}
	c.member = func(m model.MemberEvidence) []issue {
		if m.ChecksumSHA256 != "" {
			return nil
		}
		is := memberIssue(m, "CHECKSUM_MISSING", "the object version has no recorded SHA-256 checksum")
		is.Remediation = "upload the object again with its checksum"
		return []issue{is}
	}
	return nil
}

func compileChecksumVerified(c *check, config json.RawMessage) error {
	var cfg struct {
		// RequireDeclared accepts only checksums the client declared and
		// processing matched; a computed one proves the stored bytes only.
		RequireDeclared bool `json:"require_declared"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	c.member = func(m model.MemberEvidence) []issue {
		switch {
		case m.VerificationStatus == "FAILED":
			return []issue{memberIssue(m, "CHECKSUM_VERIFICATION_FAILED", "processing found the bytes did not match the checksum")}
		case m.VerificationStatus != "PASSED":
			is := memberIssue(m, "CHECKSUM_UNVERIFIED", "there is no record of the checksum being verified")
			is.Remediation = "run a FULL validation to verify the bytes"
			return []issue{is}
		case cfg.RequireDeclared && m.ChecksumSource != "DECLARED":
			is := memberIssue(m, "CHECKSUM_NOT_DECLARED", "the checksum was computed by the platform, not declared by the uploader")
			is.Expected, is.Observed = "DECLARED", m.ChecksumSource
			return []issue{is}
		}
		return nil
	}
	return nil
}

func compileMalwareScan(c *check, config json.RawMessage) error {
	var cfg struct {
		// Policy "clean" needs a CLEAN verdict; "scanned" accepts any
		// completed scan that found nothing. INFECTED always fails.
		Policy string `json:"policy"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if cfg.Policy == "" {
		cfg.Policy = "clean"
	}
	if cfg.Policy != "clean" && cfg.Policy != "scanned" {
		return fmt.Errorf(`policy: "clean" or "scanned"`)
	}
	c.member = func(m model.MemberEvidence) []issue {
		switch {
		case m.ScanResult == "INFECTED":
			is := memberIssue(m, "MALWARE_DETECTED", "the malware scan found a threat")
			is.Remediation = "remove the member and delete the object"
			return []issue{is}
		case m.ScanResult == "":
			return []issue{memberIssue(m, "SCAN_MISSING", "there is no malware scan record for the version")}
		case cfg.Policy == "clean" && m.ScanResult != "CLEAN":
			is := memberIssue(m, "NOT_SCANNED_CLEAN", "the version has no CLEAN malware scan verdict")
			is.Expected, is.Observed = "CLEAN", m.ScanResult
			return []issue{is}
		case cfg.Policy == "scanned" && m.ScanResult != "CLEAN" && m.ScanResult != "NOT_SCANNED" && m.ScanResult != "UNSUPPORTED":
			is := memberIssue(m, "SCAN_INCOMPLETE", "the malware scan did not complete")
			is.Observed = m.ScanResult
			return []issue{is}
		}
		return nil
	}
	return nil
}

// compileContentChecks fails members whose upload content checks found
// something (active content, unsafe model files, secrets, broken formats,
// malware found on a re-scan), and, unless allow_unchecked is set, members
// the checks never ran on.
func compileContentChecks(c *check, config json.RawMessage) error {
	var cfg struct {
		// Ignore lists finding families or codes that do not fail a member.
		Ignore         []string `json:"ignore"`
		AllowUnchecked bool     `json:"allow_unchecked"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	ignored := func(code string) bool {
		family, _, _ := strings.Cut(code, ".")
		return slices.Contains(cfg.Ignore, code) || slices.Contains(cfg.Ignore, family)
	}
	c.member = func(m model.MemberEvidence) []issue {
		var out []issue
		for _, f := range m.ContentFindings {
			action, code, _ := strings.Cut(f, ":")
			if ignored(code) {
				continue
			}
			is := memberIssue(m, "CONTENT_FLAGGED", "a content check flagged the file: "+code)
			if action == model.FindingReject {
				is = memberIssue(m, "CONTENT_REJECTED", "a content check rejected the file: "+code)
				is.Remediation = "remove the member; the file is no longer served"
			}
			is.Observed = code
			out = append(out, is)
		}
		if len(out) == 0 && m.ContentChecks == "" && !cfg.AllowUnchecked {
			out = append(out, memberIssue(m, "CONTENT_NOT_CHECKED", "the content checks did not run on this file's upload"))
		}
		return out
	}
	return nil
}

// compileStorageIntegrity fails members whose stored object changed
// outside the platform (found by the drift scan), and, unless
// allow_posture_fail is set, members on a connection failing its posture
// checks.
func compileStorageIntegrity(c *check, config json.RawMessage) error {
	var cfg struct {
		AllowPostureFail bool `json:"allow_posture_fail"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	c.member = func(m model.MemberEvidence) []issue {
		var out []issue
		for _, kind := range m.StorageDrift {
			is := memberIssue(m, "STORAGE_CHANGED_OUTSIDE", "the stored object changed outside the platform: "+kind)
			is.Observed = kind
			is.Remediation = "check the object against the member's SHA-256, restore the pinned version, then resolve the finding"
			out = append(out, is)
		}
		if m.ConnectionPosture == model.PostureFail && !cfg.AllowPostureFail {
			is := memberIssue(m, "STORAGE_POSTURE_FAILING", "the member's storage connection fails its posture checks")
			is.Remediation = "fix what the connection's posture report lists"
			out = append(out, is)
		}
		return out
	}
	return nil
}

// compileResidency fails members stored outside the locations the
// dataset allows (or the rule's own "locations", when set), and members
// whose connection's location the provider never confirmed.
func compileResidency(c *check, config json.RawMessage) error {
	var cfg struct {
		Locations []string `json:"locations"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	own, ok := model.NormalizeLocations(cfg.Locations)
	if !ok {
		return fmt.Errorf("%w: residency locations are provider location codes such as ap-south-1 or eu-*", model.ErrInvalidProfile)
	}
	// A dataset that allows anywhere gives the rule nothing to check.
	c.applies = func(m model.MemberEvidence) bool { return len(own) > 0 || len(m.DatasetResidency) > 0 }
	c.member = func(m model.MemberEvidence) []issue {
		allowed := own
		if len(allowed) == 0 {
			allowed = m.DatasetResidency
		}
		where := m.ConnectionLocation
		if where == "" {
			where = "an unknown location"
		}
		switch {
		case !m.LocationVerified:
			is := memberIssue(m, "RESIDENCY_UNVERIFIED", "the member is stored in "+where+", which the provider has not confirmed")
			is.Observed, is.Expected = m.ConnectionLocation, strings.Join(allowed, ", ")
			is.Remediation = "test its storage connection with credentials that may read the bucket's location"
			return []issue{is}
		case !model.LocationAllowed(m.ConnectionLocation, allowed):
			is := memberIssue(m, "RESIDENCY_OUTSIDE", "the member is stored in "+where+", outside the allowed locations")
			is.Observed, is.Expected = m.ConnectionLocation, strings.Join(allowed, ", ")
			is.Remediation = "copy it to a connection in an allowed location and remove this copy"
			return []issue{is}
		}
		return nil
	}
	return nil
}

func compileContentChecksum(c *check, config json.RawMessage) error {
	var cfg contentRule
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	c.content = &cfg
	return nil
}

// ─── Dataset level ────────────────────────────────────────────────────

// countRange is an optional [min, max] bound; 0 leaves a side open.
type countRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

func (r countRange) valid() bool {
	return r.Min >= 0 && r.Max >= 0 && (r.Max == 0 || r.Max >= r.Min)
}

func compileMemberCount(c *check, config json.RawMessage) error {
	var cfg countRange
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if !cfg.valid() {
		return fmt.Errorf("min and max: a non-negative range")
	}
	if cfg.Min == 0 {
		cfg.Min = 1 // an empty dataset is never ready
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		observed := strconv.FormatInt(a.Members, 10)
		switch {
		case a.Members == 0:
			return []issue{{Code: "EMPTY_DATASET", Message: "the dataset version has no members", Observed: "0", Key: "count"}}, ""
		case a.Members < cfg.Min:
			return []issue{{Code: "TOO_FEW_MEMBERS", Message: "the version has fewer members than the profile needs",
				Expected: fmt.Sprintf(">= %d", cfg.Min), Observed: observed, Key: "count"}}, ""
		case cfg.Max > 0 && a.Members > cfg.Max:
			return []issue{{Code: "TOO_MANY_MEMBERS", Message: "the version has more members than the profile allows",
				Expected: fmt.Sprintf("<= %d", cfg.Max), Observed: observed, Key: "count"}}, ""
		}
		return nil, ""
	}
	return nil
}

func compileTotalBytes(c *check, config json.RawMessage) error {
	var cfg countRange
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if !cfg.valid() || (cfg.Min == 0 && cfg.Max == 0) {
		return fmt.Errorf("min and max: a non-negative range, at least one of them set")
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		if a.TotalBytes >= cfg.Min && (cfg.Max == 0 || a.TotalBytes <= cfg.Max) {
			return nil, ""
		}
		return []issue{{Code: "TOTAL_BYTES_OUT_OF_RANGE", Message: "the version's total size is outside the profile's range",
			Expected: fmt.Sprintf("%d..%d bytes", cfg.Min, cfg.Max), Observed: strconv.FormatInt(a.TotalBytes, 10), Key: "bytes"}}, ""
	}
	return nil
}

// compileSmallFiles warns about a version of mostly small files that are
// not packed: read one by one, each costs a request's time to first byte,
// and data loaders starve the GPUs. Packed files stream a pack at a time.
func compileSmallFiles(c *check, config json.RawMessage) error {
	cfg := struct {
		MedianBelow int64   `json:"median_below"`
		PackedShare float64 `json:"packed_share"`
	}{MedianBelow: 1 << 20, PackedShare: 0.5}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if cfg.MedianBelow <= 0 || cfg.PackedShare < 0 || cfg.PackedShare > 1 {
		return fmt.Errorf("median_below is a positive size and packed_share a share from 0 to 1")
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		if a.Members == 0 || a.MedianSize >= cfg.MedianBelow || float64(a.PackedMembers) >= cfg.PackedShare*float64(a.Members) {
			return nil, ""
		}
		return []issue{{Code: "SMALL_FILES_UNPACKED", Key: "median_bytes",
			Message:  "most files are small and few are packed, so reading them one by one is slow",
			Expected: fmt.Sprintf("a median of at least %d bytes, or %.0f%% of files packed", cfg.MedianBelow, cfg.PackedShare*100),
			Observed: fmt.Sprintf("a median of %d bytes, %d of %d files packed", a.MedianSize, a.PackedMembers, a.Members),
			Remediation: "read it with shard streams (one request per pack); packing takes small files after they settle, " +
				"and a repack gathers the dataset's packs in path order"}}, ""
	}
	return nil
}

func compileDuplicateReferences(c *check, config json.RawMessage) error {
	if err := decodeConfig(config, &struct{}{}); err != nil {
		return err
	}
	// A version holds each object once (a unique index), and a version
	// belongs to one object; this checks that the database agrees.
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		if a.DistinctAssets == a.Members {
			return nil, ""
		}
		return []issue{{Code: "DUPLICATE_REFERENCE", Message: "an object version is referenced more than once",
			Expected: strconv.FormatInt(a.Members, 10), Observed: strconv.FormatInt(a.DistinctAssets, 10), Key: "references"}}, ""
	}
	return nil
}

func groupIssues(code, message string, groups []model.ChecksumGroup, total int64) []issue {
	out := make([]issue, 0, len(groups))
	for _, g := range groups {
		out = append(out, issue{
			Code: code, Message: message, Key: g.Checksum,
			Observed:    fmt.Sprintf("%d members share sha256 %s (e.g. %s); splits: %s", g.Members, g.Checksum, strings.Join(g.ObjectIDs, ","), strings.Join(g.Splits, ",")),
			Remediation: "keep one copy of the content, or move the copies into one split",
		})
	}
	if extra := total - int64(len(groups)); extra > 0 {
		out = append(out, issue{Code: code + "_MORE", Message: fmt.Sprintf("%d more groups are not listed", extra), Key: "more"})
	}
	return out
}

func compileDuplicateContent(c *check, config json.RawMessage) error {
	if err := decodeConfig(config, &struct{}{}); err != nil {
		return err
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		note := fmt.Sprintf("compared %d of %d members with verified checksums", a.VerifiedChecksums, a.Members)
		return groupIssues("DUPLICATE_CONTENT", "members with identical verified content", a.Duplicates, a.DuplicateGroups), note
	}
	return nil
}

// compileSplitLeakage: leakage is the same content in two splits, proven
// by equal SHA-256 checksums that processing verified against the stored
// bytes. Unverified checksums, file names and paths are never taken as
// proof; members without a verified checksum are not assessed.
func compileSplitLeakage(c *check, config json.RawMessage) error {
	if err := decodeConfig(config, &struct{}{}); err != nil {
		return err
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		note := fmt.Sprintf("compared %d of %d members with verified checksums", a.VerifiedChecksums, a.Members)
		return groupIssues("SPLIT_LEAKAGE", "identical verified content in more than one split", a.Leaks, a.LeakGroups), note
	}
	return nil
}

func compileSplitProportions(c *check, config json.RawMessage) error {
	var cfg struct {
		// Proportions bound each split's share of the members, [min, max].
		Proportions map[string][2]float64 `json:"proportions"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Proportions) == 0 {
		return fmt.Errorf("proportions: at least one split")
	}
	for s, r := range cfg.Proportions {
		if !slices.Contains(model.DatasetSplits, s) || r[0] < 0 || r[1] > 1 || r[0] > r[1] {
			return fmt.Errorf("proportions.%s: a split with 0 <= min <= max <= 1", s)
		}
	}
	splits := make([]string, 0, len(cfg.Proportions))
	for s := range cfg.Proportions {
		splits = append(splits, s)
	}
	slices.Sort(splits)
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		if a.Members == 0 {
			return nil, "no members"
		}
		var out []issue
		for _, s := range splits {
			share := float64(countOf(a.Splits, s)) / float64(a.Members)
			r := cfg.Proportions[s]
			if share < r[0] || share > r[1] {
				out = append(out, issue{Code: "SPLIT_PROPORTION", Message: "a split's share is outside the profile's range",
					Expected: fmt.Sprintf("%s in %.2f..%.2f", s, r[0], r[1]), Observed: fmt.Sprintf("%s %.4f", s, share), Key: s})
			}
		}
		return out, ""
	}
	return nil
}

func countOf(counts []model.Count, value string) int64 {
	for _, c := range counts {
		if c.Value == value {
			return c.Count
		}
	}
	return 0
}

// labelsComplete tells whether the aggregates list every label, so
// per-class rules can be judged.
func labelsComplete(a model.VersionAggregates) bool {
	return int64(len(a.Labels)) == a.LabelCount
}

func compileRequiredLabels(c *check, config json.RawMessage) error {
	var cfg struct {
		Labels []string `json:"labels"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if len(cfg.Labels) == 0 || len(cfg.Labels) > 1000 {
		return fmt.Errorf("labels: 1 to 1000 labels")
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		if !labelsComplete(a) {
			return nil, notAssessed(fmt.Sprintf("%d distinct labels are more than a report lists", a.LabelCount))
		}
		var out []issue
		for _, l := range cfg.Labels {
			if countOf(a.Labels, l) == 0 {
				out = append(out, issue{Code: "LABEL_MISSING", Message: "a required label has no members", Expected: l, Key: l})
			}
		}
		return out, ""
	}
	return nil
}

func compileLabelDistribution(c *check, config json.RawMessage) error {
	var cfg struct {
		MinPerClass   int64   `json:"min_per_class"`
		MaxClassShare float64 `json:"max_class_share"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if cfg.MinPerClass < 0 || cfg.MaxClassShare < 0 || cfg.MaxClassShare > 1 || (cfg.MinPerClass == 0 && cfg.MaxClassShare == 0) {
		return fmt.Errorf("min_per_class and max_class_share (0..1): at least one")
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		labelled := a.Members - a.Unlabeled
		if labelled == 0 {
			return nil, notAssessed("no member has a label")
		}
		var out []issue
		if cfg.MaxClassShare > 0 && len(a.Labels) > 0 {
			// Labels are listed most frequent first.
			top := a.Labels[0]
			if share := float64(top.Count) / float64(labelled); share > cfg.MaxClassShare {
				out = append(out, issue{Code: "CLASS_IMBALANCE", Message: "one class holds more of the labelled members than the profile allows",
					Expected: fmt.Sprintf("<= %.2f", cfg.MaxClassShare), Observed: fmt.Sprintf("%s %.4f", top.Value, share), Key: "share"})
			}
		}
		if cfg.MinPerClass > 0 {
			if !labelsComplete(a) {
				return out, notAssessed(fmt.Sprintf("%d distinct labels are more than a report lists", a.LabelCount))
			}
			for _, l := range a.Labels {
				if l.Count < cfg.MinPerClass {
					out = append(out, issue{Code: "CLASS_TOO_SMALL", Message: "a class has fewer examples than the profile needs",
						Expected: fmt.Sprintf(">= %d", cfg.MinPerClass), Observed: fmt.Sprintf("%s %d", l.Value, l.Count), Key: l.Value})
				}
			}
		}
		return out, ""
	}
	return nil
}

func compileFileTypeDistribution(c *check, config json.RawMessage) error {
	var cfg struct {
		Expected []string `json:"expected"`
		MaxShare float64  `json:"max_share"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if cfg.MaxShare < 0 || cfg.MaxShare > 1 || (len(cfg.Expected) == 0 && cfg.MaxShare == 0) {
		return fmt.Errorf("expected media types or max_share (0..1): at least one")
	}
	c.dataset = func(a model.VersionAggregates) ([]issue, string) {
		if a.Members == 0 {
			return nil, "no members"
		}
		var out []issue
		for _, t := range a.ContentTypes {
			if len(cfg.Expected) > 0 && !mediaTypeAllowed(t.Value, cfg.Expected) {
				out = append(out, issue{Code: "UNEXPECTED_FILE_TYPE", Message: "members of a type the profile does not expect",
					Expected: strings.Join(cfg.Expected, ","), Observed: fmt.Sprintf("%s %d", t.Value, t.Count), Key: "type:" + t.Value})
			}
			if share := float64(t.Count) / float64(a.Members); cfg.MaxShare > 0 && share > cfg.MaxShare {
				out = append(out, issue{Code: "FILE_TYPE_DOMINANT", Message: "one file type holds more of the members than the profile allows",
					Expected: fmt.Sprintf("<= %.2f", cfg.MaxShare), Observed: fmt.Sprintf("%s %.4f", t.Value, share), Key: "share:" + t.Value})
			}
		}
		return out, ""
	}
	return nil
}

// notAssessedPrefix marks a dataset check's note that says it could not
// judge the version.
const notAssessedPrefix = "not assessed: "

func notAssessed(reason string) string { return notAssessedPrefix + reason }
