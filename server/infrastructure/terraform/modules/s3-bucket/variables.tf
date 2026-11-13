variable "abort_incomplete_upload_days" {
  description = "Days after which incomplete multipart uploads are aborted"
  type        = number
  default     = 7
}

variable "access_log_bucket" {
  description = "Bucket that receives server access logs; empty turns access logging off"
  type        = string
  default     = ""
}

variable "cors_allowed_origins" {
  description = "Origins allowed to upload and read with presigned URLs from a browser; empty sets no CORS"
  type        = list(string)
  default     = []
}

variable "kms_key_arn" {
  description = "KMS key for default encryption"
  type        = string
}

variable "name" {
  description = "Bucket name prefix; a random suffix is added"
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,50}$", var.name))
    error_message = "name must be lowercase letters, digits and hyphens, at most 51 characters."
  }
}

variable "object_lock" {
  description = "Create the bucket with Object Lock, so legal holds bind the bucket too (implies versioning; cannot be turned off later)"
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags applied to the bucket"
  type        = map(string)
  default     = {}
}

variable "versioning" {
  description = "Keep object versions (dataset snapshots pin exact versions)"
  type        = bool
  default     = false
}
