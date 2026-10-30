package dataset

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Built-in profiles live in code, version 1 each; a change to one ships
// as a new version, so jobs that used the old one stay reproducible (a
// job also keeps the definition it ran with). Their IDs start with
// "builtin:". They check what metadata and processing evidence can show:
// none of them judges what a file means, how good a label is, or whether
// a model will train well on the data.
const builtinPrefix = "builtin:"

func rule(id, typ string, severity model.Severity, config string) model.Rule {
	r := model.Rule{ID: id, Version: 1, Type: typ, Severity: severity, Enabled: true, Scope: ruleTypes[typ].scope}
	if config != "" {
		r.Config = json.RawMessage(config)
	}
	return r
}

// generalRules apply to any file dataset: every version exists and can be
// read, its records agree with the snapshot, its checksum was verified and
// its scan was clean, and content is not duplicated.
func generalRules() []model.Rule {
	return []model.Rule{
		rule("object-exists", "object_exists", model.SeverityError, ""),
		rule("lifecycle-state", "lifecycle_state", model.SeverityError, ""),
		rule("size-consistency", "size_consistency", model.SeverityError, ""),
		rule("checksum-present", "checksum_present", model.SeverityWarning, ""),
		rule("checksum-verified", "checksum_verified", model.SeverityWarning, ""),
		rule("malware-scan", "malware_scan", model.SeverityError, `{"policy":"clean"}`),
		rule("content-checksum", "content_checksum", model.SeverityError, ""),
		rule("member-count", "member_count", model.SeverityError, `{"min":1}`),
		rule("duplicate-references", "duplicate_references", model.SeverityError, ""),
		rule("duplicate-content", "duplicate_content", model.SeverityWarning, ""),
	}
}

func mediaProfile(types, extensions string) []model.Rule {
	return append(generalRules(),
		rule("media-types", "allowed_media_types", model.SeverityError, `{"types":`+types+`}`),
		rule("extensions", "allowed_extensions", model.SeverityWarning, `{"extensions":`+extensions+`}`),
		rule("file-types", "file_type_distribution", model.SeverityInfo, `{"max_share":1}`),
	)
}

var builtinProfiles = func() []model.ValidationProfile {
	profiles := []model.ValidationProfile{
		{ID: "builtin:general", Name: "General file dataset", Description: "Existence, accessibility, integrity evidence, malware scans and duplicates for any files.",
			Definition: model.ProfileDefinition{Kind: "general", Rules: generalRules()}},
		{ID: "builtin:image", Name: "Image dataset", Description: "The general checks, and image media types and extensions.",
			Definition: model.ProfileDefinition{Kind: "image", Rules: mediaProfile(`["image/*"]`,
				`["jpg","jpeg","png","gif","webp","bmp","tif","tiff"]`)}},
		{ID: "builtin:audio", Name: "Audio dataset", Description: "The general checks, and audio media types and extensions.",
			Definition: model.ProfileDefinition{Kind: "audio", Rules: mediaProfile(`["audio/*"]`,
				`["wav","mp3","flac","ogg","m4a","aac","opus"]`)}},
		{ID: "builtin:video", Name: "Video dataset", Description: "The general checks, and video media types and extensions.",
			Definition: model.ProfileDefinition{Kind: "video", Rules: mediaProfile(`["video/*"]`,
				`["mp4","mov","mkv","webm","avi"]`)}},
		{ID: "builtin:text", Name: "Text or document dataset", Description: "The general checks, and text and document media types and extensions.",
			Definition: model.ProfileDefinition{Kind: "text", Rules: mediaProfile(
				`["text/*","application/pdf","application/json","application/x-ndjson","application/xml"]`,
				`["txt","md","csv","tsv","json","jsonl","pdf","html","xml"]`)}},
		{ID: "builtin:supervised-classification", Name: "Supervised classification dataset",
			Description: "The general checks, and labels, train/validation/test splits, split leakage by verified content, and class balance.",
			Definition: model.ProfileDefinition{Kind: "supervised-classification", Rules: append(generalRules(),
				rule("label-required", "required_label", model.SeverityError, ""),
				rule("split-required", "split_assignment", model.SeverityError, ""),
				rule("split-leakage", "split_leakage", model.SeverityError, ""),
				rule("split-proportions", "split_proportions", model.SeverityWarning,
					`{"proportions":{"train":[0.5,0.9],"validation":[0.05,0.3],"test":[0.05,0.3]}}`),
				rule("class-balance", "label_distribution", model.SeverityWarning, `{"min_per_class":1,"max_class_share":0.9}`),
			)}},
		// Run in FULL mode: every pinned file is read back from storage and
		// hashed again, even when processing verified it at upload. It is
		// the check for bytes changed, removed or corrupted after the
		// snapshot, including out-of-band edits in a customer's own bucket
		// that the platform could not see.
		{ID: "builtin:storage-audit", Name: "Storage audit (re-read every file)",
			Description: "Reads every pinned file back from storage and checks its size and SHA-256 again; finds files changed, missing or corrupted since the snapshot. Run it in FULL mode.",
			Definition: model.ProfileDefinition{Kind: "storage-audit", Rules: []model.Rule{
				rule("object-exists", "object_exists", model.SeverityError, ""),
				rule("lifecycle-state", "lifecycle_state", model.SeverityError, ""),
				rule("size-consistency", "size_consistency", model.SeverityError, ""),
				rule("content-reread", "content_checksum", model.SeverityError, `{"reverify":true}`),
			}}},
		// Training data with no licence or source on record is a legal risk
		// the platform can name before training, not after.
		{ID: "builtin:provenance", Name: "Provenance and licence",
			Description: "The general checks, and every file must carry license and source metadata.",
			Definition: model.ProfileDefinition{Kind: "provenance", Rules: append(generalRules(),
				rule("license-and-source", "governance_metadata", model.SeverityError, `{"keys":["license","source"]}`),
			)}},
	}
	for i := range profiles {
		profiles[i].Version = 1
		profiles[i].BuiltIn = true
		profiles[i].CreatedBy = "platform"
	}
	return profiles
}()

// Version 2 of the built-in profiles adds the content-checks rule wherever
// malware scans are checked. Version 1 stays resolvable, so the reports
// that used it can still be read against what they were judged by.
var builtinProfilesV2 = func() []model.ValidationProfile {
	var out []model.ValidationProfile
	for _, p := range builtinProfiles {
		p.Version = 2
		rules := slices.Clone(p.Definition.Rules)
		if slices.ContainsFunc(rules, func(r model.Rule) bool { return r.Type == "malware_scan" }) {
			rules = append(rules, rule("content-checks", "content_checks", model.SeverityWarning, ""))
		}
		p.Definition.Rules = rules
		out = append(out, p)
	}
	return out
}()

// Version 3 adds the storage-integrity rule beside the content checks:
// members changed outside the platform, and connections failing posture.
var builtinProfilesV3 = func() []model.ValidationProfile {
	var out []model.ValidationProfile
	for _, p := range builtinProfilesV2 {
		p.Version = 3
		rules := slices.Clone(p.Definition.Rules)
		if slices.ContainsFunc(rules, func(r model.Rule) bool { return r.Type == "content_checks" }) {
			rules = append(rules, rule("storage-integrity", "storage_integrity", model.SeverityError, ""))
		}
		p.Definition.Rules = rules
		out = append(out, p)
	}
	return out
}()

// Version 4 adds the residency rule: members stored outside the locations
// their dataset allows. It finds nothing for datasets that allow anywhere.
var builtinProfilesV4 = func() []model.ValidationProfile {
	var out []model.ValidationProfile
	for _, p := range builtinProfilesV3 {
		p.Version = 4
		p.Definition.Rules = append(slices.Clone(p.Definition.Rules), rule("residency", "residency", model.SeverityError, ""))
		out = append(out, p)
	}
	return out
}()

// Version 5 adds the small-files warning: a version of mostly small,
// unpacked files that a data loader reads slowly.
var builtinProfilesV5 = func() []model.ValidationProfile {
	var out []model.ValidationProfile
	for _, p := range builtinProfilesV4 {
		p.Version = 5
		p.Definition.Rules = append(slices.Clone(p.Definition.Rules), rule("small-files", "small_files", model.SeverityWarning, ""))
		out = append(out, p)
	}
	return out
}()

// BuiltinProfiles lists the latest version of each built-in profile.
func BuiltinProfiles() []model.ValidationProfile { return slices.Clone(builtinProfilesV5) }

func builtinProfile(id string, version int) (model.ValidationProfile, bool) {
	for _, list := range [][]model.ValidationProfile{builtinProfilesV5, builtinProfilesV4, builtinProfilesV3, builtinProfilesV2, builtinProfiles} {
		for _, p := range list {
			if p.ID == id && (version == 0 || version == p.Version) {
				return p, true
			}
		}
	}
	return model.ValidationProfile{}, false
}

// profile finds a profile by ID: a built-in one, or one of the tenant's.
func (s *Service) profile(ctx context.Context, tenantID, id string, version int) (model.ValidationProfile, error) {
	if strings.HasPrefix(id, builtinPrefix) {
		p, ok := builtinProfile(id, version)
		if !ok {
			return p, model.ErrProfileNotFound
		}
		return p, nil
	}
	return s.deps.Repo.GetProfile(ctx, tenantID, id, version)
}

// ProfileInput describes a tenant's profile.
type ProfileInput struct {
	Name        string
	Description string
	Definition  model.ProfileDefinition
}

// CreateProfile writes a tenant profile: version 1 of a new name, or the
// name's next version. Written versions never change.
func (s *Service) CreateProfile(ctx context.Context, p model.Principal, in ProfileInput) (model.ValidationProfile, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "validation_profile.create", "validation_profile", in.Name); err != nil {
		return model.ValidationProfile{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 128 || len(in.Description) > 2000 || hasControl(in.Name) {
		return model.ValidationProfile{}, fmt.Errorf("%w: a name of 1-128 characters and a description of at most 2000", ErrInvalidRequest)
	}
	if _, err := compile(in.Definition); err != nil {
		return model.ValidationProfile{}, err
	}
	for i, r := range in.Definition.Rules {
		if r.Scope == "" {
			in.Definition.Rules[i].Scope = ruleTypes[r.Type].scope
		}
	}
	if raw, _ := json.Marshal(in.Definition); len(raw) > 64*1024 {
		return model.ValidationProfile{}, fmt.Errorf("%w: the definition is larger than 64 KiB", model.ErrInvalidProfile)
	}
	stored, err := s.deps.Repo.CreateProfile(ctx, model.ValidationProfile{
		ID: "vp_" + event.NewID(), TenantID: p.TenantID, Name: in.Name, Description: in.Description,
		Definition: in.Definition, CreatedBy: p.UserID, CreatedAt: s.now(),
	})
	if err == nil {
		s.record(ctx, p, "validation_profile.create", "validation_profile", stored.ID, map[string]any{
			"name": stored.Name, "version": stored.Version, "rules": len(stored.Definition.Rules)})
	}
	return stored, err
}

// Profiles lists the built-in profiles and the latest version of each of
// the tenant's.
func (s *Service) Profiles(ctx context.Context, p model.Principal) ([]model.ValidationProfile, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "validation_profile.read", "validation_profile", ""); err != nil {
		return nil, err
	}
	own, err := s.deps.Repo.ListProfiles(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	return append(BuiltinProfiles(), own...), nil
}

// Profile returns one version of a profile; version 0 is the latest.
func (s *Service) Profile(ctx context.Context, p model.Principal, id string, version int) (model.ValidationProfile, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "validation_profile.read", "validation_profile", id); err != nil {
		return model.ValidationProfile{}, err
	}
	return s.profile(ctx, p.TenantID, id, version)
}
