package storageconn

import "github.com/sanjeev0303/object-storage-platform/internal/model"

// AuthMethodInfo is one way to authenticate to a provider.
type AuthMethodInfo struct {
	Method       model.StorageAuthMethod
	SecretFields []string
}

// ProviderInfo describes a provider: how to connect to it and what the
// platform's adapter for it implements. A connection test reports what a
// particular bucket actually allows (versioning, for one).
type ProviderInfo struct {
	Provider       model.StorageProviderType
	Name           string
	AuthMethods    []AuthMethodInfo
	RequiredFields []string
	Capabilities   model.ProviderCapabilities
	Notes          []string
}

var s3Capabilities = model.ProviderCapabilities{
	PresignedUpload: true, MaxSingleUploadBytes: 5 << 30,
	Multipart: true, MultipartKind: "S3", MaxParts: 10000, MinPartBytes: 5 << 20, PartValidator: "CONTENT",
	ListIncompleteUploads: true, ServerSideCopy: true, RangeReads: true, ConditionalReads: true,
	PresignedDownload: true, ListObjects: true, ChecksumAlgorithms: []string{"MD5"},
}

func withVersioning(caps model.ProviderCapabilities, kind string) model.ProviderCapabilities {
	caps.VersionIDKind = kind
	return caps
}

// Providers is the provider catalog.
func Providers() []ProviderInfo {
	accessKey := AuthMethodInfo{Method: model.AuthAccessKey, SecretFields: []string{"access_key_id", "secret_access_key", "session_token (optional)"}}
	return []ProviderInfo{
		{
			Provider: model.ProviderPlatform, Name: "Platform-managed storage",
			AuthMethods:  []AuthMethodInfo{{Method: model.AuthPlatform}},
			Capabilities: withVersioning(s3Capabilities, "S3_VERSION_ID"),
			Notes:        []string{"The deployment's own buckets; every organization's initial default."},
		},
		{
			Provider: model.ProviderAWSS3, Name: "Amazon S3",
			AuthMethods:    []AuthMethodInfo{accessKey, {Method: model.AuthAssumeRole}},
			RequiredFields: []string{"bucket", "region"},
			Capabilities:   withVersioning(s3Capabilities, "S3_VERSION_ID"),
			Notes: []string{
				"ASSUME_ROLE: the platform's own AWS identity assumes role_arn with the external_id the platform generates; require it in the role's trust policy.",
				"Version IDs are used where the bucket has versioning enabled.",
			},
		},
		{
			Provider: model.ProviderR2, Name: "Cloudflare R2",
			AuthMethods:    []AuthMethodInfo{{Method: model.AuthAccessKey, SecretFields: []string{"access_key_id", "secret_access_key"}}},
			RequiredFields: []string{"bucket", "account_id"},
			Capabilities:   s3Capabilities,
			Notes:          []string{"Signed with the region \"auto\" at https://<account_id>.r2.cloudflarestorage.com; R2 has no object versioning."},
		},
		{
			Provider: model.ProviderS3Compatible, Name: "S3-compatible service",
			AuthMethods:    []AuthMethodInfo{{Method: model.AuthAccessKey, SecretFields: []string{"access_key_id", "secret_access_key"}}},
			RequiredFields: []string{"bucket", "endpoint"},
			Capabilities:   s3Capabilities,
			Notes: []string{
				"Any service speaking the S3 API at a public HTTPS endpoint. Capabilities beyond the S3 basics are confirmed by the connection test, not assumed.",
			},
		},
		{
			Provider: model.ProviderGCS, Name: "Google Cloud Storage",
			AuthMethods:    []AuthMethodInfo{{Method: model.AuthServiceAccountKey, SecretFields: []string{"service_account_json"}}},
			RequiredFields: []string{"bucket"},
			Capabilities: model.ProviderCapabilities{
				PresignedUpload: true, MaxSingleUploadBytes: 5 << 40,
				Multipart: true, MultipartKind: "GCS_COMPOSE", MaxParts: 10000, PartValidator: "CONTENT",
				ListIncompleteUploads: true, ServerSideCopy: true, RangeReads: true, ConditionalReads: true,
				PresignedDownload: true, ListObjects: true, VersionIDKind: "GCS_GENERATION",
				ChecksumAlgorithms: []string{"MD5", "CRC32C"},
			},
			Notes: []string{
				"Large uploads are parallel composite uploads: each part is an object, composed into the final object (32 sources per compose, in levels).",
				"Object generations are kept as the provider's own version identifiers.",
				"Workload identity is not supported yet; use a service account key.",
			},
		},
		{
			Provider: model.ProviderAzureBlob, Name: "Azure Blob Storage",
			AuthMethods: []AuthMethodInfo{
				{Method: model.AuthAccountKey, SecretFields: []string{"account_key"}},
				{Method: model.AuthSASToken, SecretFields: []string{"sas_token"}},
			},
			RequiredFields: []string{"bucket (the container)", "account_id (the storage account)"},
			Capabilities: model.ProviderCapabilities{
				PresignedUpload: true, MaxSingleUploadBytes: 5000 << 20,
				Multipart: true, MultipartKind: "AZURE_BLOCKS", MaxParts: 50000, PartValidator: "BLOCK_ID",
				ListIncompleteUploads: true, ServerSideCopy: true, RangeReads: true, ConditionalReads: true,
				PresignedDownload: true, ListObjects: true, VersionIDKind: "AZURE_VERSION_ID",
				ChecksumAlgorithms: []string{"MD5", "CRC64"},
			},
			Notes: []string{
				"Large uploads are block blobs; parts are checked by block ID and size, and the platform's SHA-256 check proves the bytes.",
				"A SAS token cannot sign client URLs: such a connection stages uploads on the platform (staging_mode PLATFORM) and serves downloads through the API.",
				"Microsoft Entra ID (identity-based) authentication is not supported yet.",
			},
		},
	}
}
