package request

// StorageCredentials are a connection's secrets. They are accepted only on
// create and rotate, and never returned by any API.
type StorageCredentials struct {
	AccessKeyID        string `json:"access_key_id,omitempty"`
	SecretAccessKey    string `json:"secret_access_key,omitempty"`
	SessionToken       string `json:"session_token,omitempty"`
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
	AccountKey         string `json:"account_key,omitempty"`
	SASToken           string `json:"sas_token,omitempty"`
}

// CreateStorageConnectionRequest creates a connection to the
// organization's own storage.
type CreateStorageConnectionRequest struct {
	Name           string              `json:"name"`
	Provider       string              `json:"provider"`
	AuthMethod     string              `json:"auth_method"`
	Bucket         string              `json:"bucket"`
	Region         string              `json:"region,omitempty"`
	Endpoint       string              `json:"endpoint,omitempty"`
	AccountID      string              `json:"account_id,omitempty"`
	ForcePathStyle bool                `json:"force_path_style,omitempty"`
	RoleARN        string              `json:"role_arn,omitempty"`
	StagingPrefix  string              `json:"staging_prefix,omitempty"`
	ReadyPrefix    string              `json:"ready_prefix,omitempty"`
	StagingMode    string              `json:"staging_mode,omitempty"`
	Credentials    *StorageCredentials `json:"credentials,omitempty"`
}

// UpdateStorageConnectionRequest changes non-secret settings; omitted
// fields stay as they are. Revision (or If-Match) is required.
type UpdateStorageConnectionRequest struct {
	Revision       int64   `json:"revision,omitempty"`
	Name           *string `json:"name,omitempty"`
	Region         *string `json:"region,omitempty"`
	Endpoint       *string `json:"endpoint,omitempty"`
	ForcePathStyle *bool   `json:"force_path_style,omitempty"`
	StagingMode    *string `json:"staging_mode,omitempty"`
	// ChecksumMode is AUTO, WHEN_SUPPORTED or WHEN_REQUIRED.
	ChecksumMode *string `json:"checksum_mode,omitempty"`
	// ExpectedOwner pins the bucket's AWS account or Google project
	// number; "" unpins it.
	ExpectedOwner *string `json:"expected_owner,omitempty"`
	// EncryptionKey is the customer-managed key every write is encrypted
	// with: a KMS key ID, ARN or alias, a Cloud KMS key name, or an Azure
	// encryption scope; "" removes it.
	EncryptionKey *string `json:"encryption_key,omitempty"`
}

// TestStorageConnectionRequest asks for a connection test. WriteProbe also
// writes, reads back and deletes one small object of the platform's own.
type TestStorageConnectionRequest struct {
	WriteProbe bool `json:"write_probe,omitempty"`
}

// RotateStorageCredentialsRequest replaces a connection's credentials.
type RotateStorageCredentialsRequest struct {
	Revision    int64              `json:"revision,omitempty"`
	Credentials StorageCredentials `json:"credentials"`
}
