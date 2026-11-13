variable "allow_customer_role_assumption" {
  description = "Let the server role assume roles in customers' accounts (BYOS ASSUME_ROLE connections)"
  type        = bool
  default     = true
}

variable "bucket_arns" {
  description = "ARNs of the platform buckets the server reads and writes"
  type        = list(string)
}

variable "kms_key_arn" {
  description = "ARN of the platform key"
  type        = string
}

variable "name" {
  description = "Name prefix for the roles"
  type        = string
}

variable "queue_arns" {
  description = "ARNs of the event queues and their dead-letter queues"
  type        = list(string)
}

variable "secret_arns" {
  description = "ARNs of the Secrets Manager secrets tasks receive at start"
  type        = list(string)
}

variable "tags" {
  description = "Tags applied to the roles"
  type        = map(string)
  default     = {}
}
