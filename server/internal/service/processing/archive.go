package processing

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ArchiveLimits bound archive inspection. Every limit must be positive.
type ArchiveLimits struct {
	MaxArchiveBytes      int64
	MaxEntries           int
	MaxUncompressedBytes int64
	MaxRatio             int64
}

// ArchiveValidator inspects zip archives without extracting them to disk:
// the archive is spooled to one temp file (zip needs random access) and
// each entry is decompressed into io.Discard, so the real expanded size is
// measured rather than trusted from the headers. Archives that would need
// more than the limits allow are rejected, as are entries with absolute or
// parent-relative paths and symlinks.
type ArchiveValidator struct {
	Limits ArchiveLimits
	// TempDir is where archives are spooled; empty uses the OS default.
	TempDir string
}

func (ArchiveValidator) Name() string { return "archive" }

func (v ArchiveValidator) Validate(ctx context.Context, sample interfaces.ContentSample, _ model.ContentPolicy) (interfaces.ValidationResult, error) {
	if DetectType(sample.Head) != "application/zip" {
		return interfaces.ValidationResult{}, nil
	}
	if sample.SizeBytes > v.Limits.MaxArchiveBytes {
		return unsafeArchive("archive is larger than the inspection limit"), nil
	}

	spool, err := os.CreateTemp(v.TempDir, "archive-inspect-*")
	if err != nil {
		return interfaces.ValidationResult{}, fmt.Errorf("create archive spool: %w", err)
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	body, err := sample.Open(ctx)
	if err != nil {
		return interfaces.ValidationResult{}, fmt.Errorf("open archive: %w", err)
	}
	written, err := io.Copy(spool, io.LimitReader(body, v.Limits.MaxArchiveBytes+1))
	body.Close()
	if err != nil {
		return interfaces.ValidationResult{}, fmt.Errorf("spool archive: %w", err)
	}
	if written > v.Limits.MaxArchiveBytes {
		return unsafeArchive("archive is larger than the inspection limit"), nil
	}

	reader, err := zip.NewReader(spool, written)
	if err != nil {
		return unsafeArchive("archive cannot be read"), nil
	}
	return v.inspect(ctx, reader)
}

func (v ArchiveValidator) inspect(ctx context.Context, reader *zip.Reader) (interfaces.ValidationResult, error) {
	if len(reader.File) > v.Limits.MaxEntries {
		return unsafeArchive("archive has too many entries"), nil
	}

	var total int64
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return interfaces.ValidationResult{}, err
		}
		if !safeEntryName(entry.Name) {
			return unsafeArchive("archive entry has an unsafe path"), nil
		}
		if entry.Mode()&fs.ModeSymlink != 0 {
			return unsafeArchive("archive contains a symlink"), nil
		}
		if entry.FileInfo().IsDir() {
			continue
		}

		size, err := expandedSize(entry, v.Limits.MaxUncompressedBytes-total)
		if errors.Is(err, errTooLarge) {
			return unsafeArchive("archive expands beyond the size limit"), nil
		}
		if err != nil {
			return unsafeArchive("archive entry cannot be read"), nil
		}
		total += size

		compressed := int64(entry.CompressedSize64)
		if compressed > 0 && size/compressed > v.Limits.MaxRatio {
			return unsafeArchive("archive entry compression ratio is too high"), nil
		}
	}
	return interfaces.ValidationResult{}, nil
}

var errTooLarge = errors.New("entry expands beyond the limit")

// expandedSize decompresses one entry, stopping as soon as it passes limit.
func expandedSize(entry *zip.File, limit int64) (int64, error) {
	rc, err := entry.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	n, err := io.Copy(io.Discard, io.LimitReader(rc, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, errTooLarge
	}
	return n, nil
}

func safeEntryName(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return false
	}
	for _, part := range strings.Split(path.Clean(name), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func unsafeArchive(detail string) interfaces.ValidationResult {
	return interfaces.ValidationResult{DetectedType: "application/zip", RejectReason: model.RejectUnsafeArchive, Detail: detail}
}
