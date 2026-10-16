package interfaces

// FileValidator judges a stored file by its own bytes. Extensions and the
// client's declared content type are untrusted; head is the start of the
// object (at most a few KiB), never the whole file.
type FileValidator interface {
	// Validate returns the detected content type, and a rejection reason
	// (one of the model.Reject* codes) when the file must not be accepted.
	Validate(head []byte, declaredContentType string) (detected string, rejectReason string)
}
