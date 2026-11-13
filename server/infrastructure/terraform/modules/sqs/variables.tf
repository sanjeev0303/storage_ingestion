variable "kms_key_arn" {
  description = "KMS key that encrypts the queues"
  type        = string
}

variable "max_receive_count" {
  description = "Receives before a message moves to its dead-letter queue (SQS_MAX_RECEIVE_COUNT)"
  type        = number
  default     = 20
}

variable "message_retention_seconds" {
  description = "How long a message waits in a queue (the outbox remains the source of truth)"
  type        = number
  default     = 345600
}

variable "prefix" {
  description = "Queue name prefix (SQS_QUEUE_PREFIX)"
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_-]*$", var.prefix))
    error_message = "prefix may contain letters, digits, hyphens and underscores only."
  }
}

variable "queues" {
  description = "Logical queue names the services publish to and consume"
  type        = list(string)
  default     = ["datasets", "ingestion", "lifecycle", "packing", "processing"]
}

variable "tags" {
  description = "Tags applied to the queues"
  type        = map(string)
  default     = {}
}

variable "visibility_timeout_seconds" {
  description = "Default visibility timeout; consumers set their own on each receive (EVENT_VISIBILITY)"
  type        = number
  default     = 60
}
