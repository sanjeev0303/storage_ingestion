package catalog

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// A cursor is where a listing continues: the sort key of the last object
// returned, the order, and a fingerprint of the tenant and the filters.
// It is opaque to clients (base64 of JSON) but not secret: it only names
// a position in the holder's own listing. A cursor used with another
// order, other filters or by another tenant is refused, so it can never
// skip a filter or move a listing into another tenant's objects; and
// since the position is re-applied as a keyset predicate, a page after
// rows were added or removed continues in order without repeating rows.
type cursor struct {
	Version     int    `json:"v"`
	Order       string `json:"o"`
	Fingerprint string `json:"f"`
	model.CatalogPosition
}

const cursorVersion = 1

func encodeCursor(f model.CatalogFilter, pos model.CatalogPosition) string {
	raw, _ := json.Marshal(cursor{Version: cursorVersion, Order: string(f.Order), Fingerprint: fingerprint(f), CatalogPosition: pos})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(token string, f model.CatalogFilter) (model.CatalogPosition, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) > 4096 {
		return model.CatalogPosition{}, fmt.Errorf("%w: not a cursor", model.ErrInvalidCursor)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return model.CatalogPosition{}, fmt.Errorf("%w: not a cursor", model.ErrInvalidCursor)
	}
	switch {
	case c.Version != cursorVersion:
		return model.CatalogPosition{}, fmt.Errorf("%w: from another version of the API; start the listing again", model.ErrInvalidCursor)
	case c.Order != string(f.Order) || c.Fingerprint != fingerprint(f):
		return model.CatalogPosition{}, fmt.Errorf("%w: it belongs to a listing with other filters", model.ErrInvalidCursor)
	case f.Order == model.OrderByUpdated && (c.ID == "" || c.UpdatedAt.IsZero()):
		return model.CatalogPosition{}, fmt.Errorf("%w: incomplete position", model.ErrInvalidCursor)
	case f.Order == model.OrderByPath && c.PathKey == "":
		return model.CatalogPosition{}, fmt.Errorf("%w: incomplete position", model.ErrInvalidCursor)
	}
	return c.CatalogPosition, nil
}

// fingerprint identifies a listing's tenant and filters, without its
// position and page size.
func fingerprint(f model.CatalogFilter) string {
	f.After, f.Limit = nil, 0
	// encoding/json sorts map keys, so the encoding is stable.
	raw, _ := json.Marshal(f)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
