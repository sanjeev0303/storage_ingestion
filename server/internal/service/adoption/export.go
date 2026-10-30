package adoption

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Exports describe a dataset version in the formats media and archive
// tools read: an ASC MHL v2 list and a BagIt payload manifest. A
// snapshot's members already carry SHA-256 (BagIt); MHL needs digests
// it does not define SHA-256 for, so members without stored ones are read
// once and their digests kept.

// Versions reads snapshot members (the dataset repository).
type Versions interface {
	GetVersion(ctx context.Context, tenantID, versionID string) (model.DatasetVersion, error)
	ListVersionMembers(ctx context.Context, tenantID, versionID string, after int64, limit int) ([]model.VersionMember, error)
}

// DigestStore keeps computed digests (postgres.AdoptionRepository).
type DigestStore interface {
	Digests(ctx context.Context, tenantID string, assetIDs []string) (map[string]model.Digests, error)
	PutDigests(ctx context.Context, tenantID, assetID string, d model.Digests, at time.Time) error
}

// Exporter writes exports of dataset versions.
type Exporter struct {
	Versions Versions
	Digests  DigestStore
	Assets   interfaces.AssetRepository
	Packs    interfaces.PackLocator
	Router   interfaces.StorageRouter
	Platform interfaces.ObjectDelivery
	Bucket   string
	Audit    interfaces.AuditRecorder
}

func (x Exporter) version(ctx context.Context, p model.Principal, datasetID, versionID, action string) (model.DatasetVersion, error) {
	if err := audit.Require(ctx, x.Audit, p, model.PermDatasetsRead, action, "dataset_version", versionID); err != nil {
		return model.DatasetVersion{}, err
	}
	v, err := x.Versions.GetVersion(ctx, p.TenantID, versionID)
	if err != nil {
		return v, err
	}
	if v.DatasetID != datasetID {
		return v, model.ErrDatasetVersionNotFound
	}
	return v, nil
}

// BagManifest writes manifest-sha256.txt for the version's payload:
// "<sha256>  data/<path>", one line per member.
func (x Exporter) BagManifest(ctx context.Context, p model.Principal, datasetID, versionID string, w io.Writer) error {
	if _, err := x.version(ctx, p, datasetID, versionID, "dataset.export_bagit"); err != nil {
		return err
	}
	out := bufio.NewWriter(w)
	escape := strings.NewReplacer("%", "%25", "\n", "%0A", "\r", "%0D")
	for after := int64(0); ; {
		page, err := x.Versions.ListVersionMembers(ctx, p.TenantID, versionID, after, 1000)
		if err != nil {
			return err
		}
		for _, m := range page {
			fmt.Fprintf(out, "%s  data/%s\n", m.ChecksumSHA256, escape.Replace(m.Path))
			after = m.Ordinal
		}
		if len(page) < 1000 {
			return out.Flush()
		}
	}
}

type mhlOut struct {
	XMLName xml.Name    `xml:"hashlist"`
	Version string      `xml:"version,attr"`
	NS      string      `xml:"xmlns,attr"`
	Creator mhlCreator  `xml:"creatorinfo"`
	Process mhlProcess  `xml:"processinfo"`
	Hashes  []mhlOutRow `xml:"hashes>hash"`
}

type mhlCreator struct {
	Date     string `xml:"creationdate"`
	Hostname string `xml:"hostname"`
	Tool     struct {
		Version string `xml:"version,attr"`
		Name    string `xml:",chardata"`
	} `xml:"tool"`
}

type mhlProcess struct {
	Process string `xml:"process"`
}

type mhlDigest struct {
	Action string `xml:"action,attr"`
	Date   string `xml:"hashdate,attr"`
	Value  string `xml:",chardata"`
}

type mhlOutRow struct {
	Path struct {
		Size  int64  `xml:"size,attr"`
		Value string `xml:",chardata"`
	} `xml:"path"`
	MD5   *mhlDigest `xml:"md5,omitempty"`
	XXH64 *mhlDigest `xml:"xxh64,omitempty"`
}

// MHL writes an ASC MHL v2 list of the version's members with their MD5
// and xxh64, each taken from a read of the stored bytes (at adoption, or
// now when none was kept).
func (x Exporter) MHL(ctx context.Context, p model.Principal, datasetID, versionID string, w io.Writer) error {
	if _, err := x.version(ctx, p, datasetID, versionID, "dataset.export_mhl"); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	list := mhlOut{Version: "2.0", NS: "urn:ASC:MHL:v2.0", Process: mhlProcess{Process: "in-place"}}
	list.Creator.Date, list.Creator.Hostname = now, "object-storage-platform"
	list.Creator.Tool.Version, list.Creator.Tool.Name = "1", "object-storage-platform"
	for after := int64(0); ; {
		page, err := x.Versions.ListVersionMembers(ctx, p.TenantID, versionID, after, 500)
		if err != nil {
			return err
		}
		ids := make([]string, len(page))
		for i, m := range page {
			ids[i] = m.AssetID
		}
		stored, err := x.Digests.Digests(ctx, p.TenantID, ids)
		if err != nil {
			return err
		}
		for _, m := range page {
			after = m.Ordinal
			d := stored[m.AssetID]
			if d["md5"] == "" || d["xxh64"] == "" {
				if d, err = x.compute(ctx, p.TenantID, m); err != nil {
					return fmt.Errorf("read %s: %w", m.Path, err)
				}
			}
			row := mhlOutRow{}
			row.Path.Size, row.Path.Value = m.SizeBytes, m.Path
			row.MD5 = &mhlDigest{Action: "original", Date: now, Value: d["md5"]}
			row.XXH64 = &mhlDigest{Action: "original", Date: now, Value: d["xxh64"]}
			list.Hashes = append(list.Hashes, row)
		}
		if len(page) < 500 {
			break
		}
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(list)
}

// compute reads a member's pinned bytes once, checks them against its
// SHA-256 and keeps their digests.
func (x Exporter) compute(ctx context.Context, tenantID string, m model.VersionMember) (model.Digests, error) {
	asset, err := x.Assets.GetByID(ctx, tenantID, m.AssetID)
	if err != nil {
		return nil, err
	}
	reader, bucket := x.Platform, x.Bucket
	if !model.IsPlatformConnection(asset.StorageConnectionID) {
		loc, err := x.Router.Locate(ctx, tenantID, asset.StorageConnectionID)
		if err != nil {
			return nil, err
		}
		reader, bucket = loc.Storage, loc.ReadyBucket
	}
	key, first, last := asset.ObjectKey, int64(0), int64(-1)
	cond := interfaces.ReadCondition{VersionID: asset.StorageVersionID}
	if x.Packs != nil {
		loc, packed, err := x.Packs.PackedLocation(ctx, tenantID, asset.ID)
		if err != nil {
			return nil, err
		}
		if packed {
			if first, last, err = loc.Range(0, -1); err != nil {
				return nil, err
			}
			bucket, key = loc.Bucket, loc.ObjectKey
			cond = interfaces.ReadCondition{IfMatch: loc.StorageETag, VersionID: loc.StorageVersionID}
		}
	}
	body, err := reader.OpenRange(ctx, bucket, key, first, last, cond)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	d := newDigester(nil)
	if _, err := io.Copy(d, body); err != nil {
		return nil, err
	}
	sums := d.Digests()
	if m.ChecksumSHA256 != "" && sums["sha256"] != m.ChecksumSHA256 {
		return nil, fmt.Errorf("the stored bytes no longer match the snapshot's SHA-256")
	}
	_ = x.Digests.PutDigests(ctx, tenantID, asset.ID, sums, time.Now().UTC())
	return sums, nil
}
