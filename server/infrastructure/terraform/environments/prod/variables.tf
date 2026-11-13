variable "admin_email" {
  description = "Email of the first platform administrator, made by scripts/bootstrap-admin.sh"
  type        = string

  validation {
    condition     = can(regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$", var.admin_email))
    error_message = "admin_email must be an email address."
  }
}

variable "alarm_emails" {
  description = "Addresses that receive alarms (each confirms a subscription email)"
  type        = list(string)
  default     = []
}

variable "alb_access_logs" {
  description = "Write load balancer access logs to a logs bucket"
  type        = bool
  default     = true
}

variable "alb_ingress_cidrs" {
  description = "Client ranges allowed to reach the load balancer"
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "api_autoscaling" {
  description = "API task count range and CPU target"
  type = object({
    min        = number
    max        = number
    cpu_target = number
  })
  default = { min = 2, max = 10, cpu_target = 60 }

  validation {
    condition     = var.api_autoscaling.min >= 2 && var.api_autoscaling.max >= var.api_autoscaling.min
    error_message = "Run at least two API tasks, and max must be at least min."
  }
}

variable "api_subdomain" {
  description = "Subdomain of domain_name the API answers on (SDKs, CLI, Git LFS)"
  type        = string
  default     = "api"
}

variable "availability_zones" {
  description = "Availability zones to run in (three for production)"
  type        = list(string)
}

variable "aws_account_id" {
  description = "Production AWS account; the provider refuses any other"
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "aws_account_id must be 12 digits."
  }
}

variable "busybox_image_tag" {
  description = "Tag of the busybox image mirrored to ECR for the admin task's init container"
  type        = string
  default     = "1.37.0"
}

variable "byos_enabled" {
  description = "Let organizations connect their own storage; also turns on the credential keyring that webhooks and SSO need"
  type        = bool
  default     = true
}

variable "certificate_key_id" {
  description = "Key ID named in signed disposition certificates (CERTIFICATE_KEY_ID)"
  type        = string
  default     = "disposition-1"
}

variable "clamav_image_tag" {
  description = "Tag of the ClamAV image mirrored to ECR for the scanner sidecar"
  type        = string
  default     = "1.4"
}

variable "console_autoscaling" {
  description = "Console task count range and CPU target"
  type = object({
    min        = number
    max        = number
    cpu_target = number
  })
  default = { min = 2, max = 6, cpu_target = 60 }
}

variable "console_subdomain" {
  description = "Subdomain of domain_name the console answers on"
  type        = string
  default     = "console"
}

variable "db_allocated_storage_gb" {
  description = "Initial database storage in GiB"
  type        = number
  default     = 100
}

variable "db_backup_retention_days" {
  description = "Days of database backups and point-in-time recovery"
  type        = number
  default     = 14
}

variable "db_instance_class" {
  description = "RDS instance class"
  type        = string
  default     = "db.r6g.large"
}

variable "db_multi_az" {
  description = "Synchronous standby in another zone"
  type        = bool
  default     = true
}

variable "db_password_version" {
  description = "Bump to generate and apply a new database password (instance and DATABASE_URL secret together)"
  type        = number
  default     = 1
}

variable "domain_name" {
  description = "Domain whose Route 53 zone holds the console and API records, e.g. example.com"
  type        = string
}

variable "image_tag" {
  description = "Image tag the services run (an immutable git commit tag, never latest)"
  type        = string

  validation {
    condition     = var.image_tag != "latest" && can(regex("^[A-Za-z0-9._-]{1,128}$", var.image_tag))
    error_message = "image_tag must be an immutable tag such as a git commit, not latest."
  }
}

variable "interface_endpoints" {
  description = "Interface VPC endpoints (keep AWS API traffic off NAT)"
  type        = list(string)
  default     = ["ecr.api", "ecr.dkr", "logs", "secretsmanager", "sqs", "sts", "kms"]
}

variable "log_retention_days" {
  description = "Days to keep task logs"
  type        = number
  default     = 90
}

variable "migration_image_tag" {
  description = "Image tag of the migration task; null uses image_tag (deploy.sh moves it first, migrates, then moves image_tag)"
  type        = string
  default     = null
}

variable "project" {
  description = "Short project name used in resource names and tags"
  type        = string
  default     = "object-storage"
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "route53_zone_id" {
  description = "Route 53 hosted zone of domain_name"
  type        = string
}

variable "self_registration" {
  description = "Allow anyone to create an account and organization (AUTH_SELF_REGISTRATION); emails are not verified"
  type        = bool
  default     = false
}

variable "server_environment_overrides" {
  description = "Extra or changed environment for the API and workers (tuning knobs from server/.env.example)"
  type        = map(string)
  default     = {}
}

variable "single_nat_gateway" {
  description = "One NAT gateway instead of one per zone (cheaper; a zone outage then cuts egress)"
  type        = bool
  default     = false
}

variable "vpc_cidr" {
  description = "VPC CIDR block"
  type        = string
  default     = "10.40.0.0/16"
}

variable "worker_overrides" {
  description = "Per-worker sizing overrides, keyed by worker name (e.g. processing-worker)"
  type = map(object({
    cpu           = optional(number)
    memory        = optional(number)
    desired_count = optional(number)
  }))
  default = {}
}
