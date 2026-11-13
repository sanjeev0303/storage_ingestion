variable "allocated_storage_gb" {
  description = "Initial storage in GiB"
  type        = number
  default     = 100
}

variable "allowed_security_groups" {
  description = "Security groups allowed to reach PostgreSQL, keyed by a fixed name (keys must be known at plan time)"
  type        = map(string)
}

variable "backup_retention_days" {
  description = "Days of automated backups and point-in-time recovery"
  type        = number
  default     = 14

  validation {
    condition     = var.backup_retention_days >= 7 && var.backup_retention_days <= 35
    error_message = "Keep 7 to 35 days of backups in production."
  }
}

variable "database_name" {
  description = "Name of the platform database"
  type        = string
  default     = "object_storage"
}

variable "engine_major_version" {
  description = "PostgreSQL major version"
  type        = string
  default     = "17"
}

variable "instance_class" {
  description = "RDS instance class"
  type        = string
  default     = "db.r6g.large"
}

variable "kms_key_arn" {
  description = "KMS key for storage and Performance Insights"
  type        = string
}

variable "master_password" {
  description = "Master password, write-only (never stored in state)"
  type        = string
  ephemeral   = true
}

variable "master_password_version" {
  description = "Bump to apply a new master password"
  type        = number
  default     = 1
}

variable "master_username" {
  description = "Master user name"
  type        = string
  default     = "storage_app"
}

variable "max_allocated_storage_gb" {
  description = "Storage autoscaling ceiling in GiB"
  type        = number
  default     = 1000
}

variable "multi_az" {
  description = "Run a synchronous standby in another zone"
  type        = bool
  default     = true
}

variable "name" {
  description = "Identifier prefix for database resources"
  type        = string
}

variable "subnet_ids" {
  description = "Database subnets (no internet route)"
  type        = list(string)
}

variable "tags" {
  description = "Tags applied to database resources"
  type        = map(string)
  default     = {}
}

variable "vpc_id" {
  description = "VPC of the database"
  type        = string
}
