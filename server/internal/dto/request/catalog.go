package request

// Catalog changes. Revision is the object's revision the client read; an
// If-Match header with it works as well.

type CatalogMetadataRequest struct {
	Revision int64              `json:"revision,omitempty"`
	Metadata map[string]*string `json:"metadata"`
}

type CatalogRenameRequest struct {
	Revision int64  `json:"revision,omitempty"`
	Path     string `json:"path"`
}

type CatalogVersioningRequest struct {
	Revision   int64  `json:"revision,omitempty"`
	Versioning string `json:"versioning"`
}

type CatalogRevisionRequest struct {
	Revision int64 `json:"revision,omitempty"`
}
