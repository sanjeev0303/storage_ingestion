variable "alarm_emails" {
  description = "Addresses subscribed to alarms (each must confirm the subscription email)"
  type        = list(string)
  default     = []
}

variable "alb_5xx_threshold" {
  description = "5xx answers in 5 minutes that raise the alarm"
  type        = number
  default     = 25
}

variable "alb_arn_suffix" {
  description = "ARN suffix of the load balancer (CloudWatch dimension)"
  type        = string
}

variable "cluster_name" {
  description = "ECS cluster name"
  type        = string
}

variable "db_identifier" {
  description = "RDS instance identifier"
  type        = string
}

variable "dead_letter_queues" {
  description = "Dead-letter queue names by logical queue"
  type        = map(string)
}

variable "kms_key_arn" {
  description = "KMS key that encrypts the alarm topic"
  type        = string
}

variable "name" {
  description = "Name prefix for alarms"
  type        = string
}

variable "queue_age_threshold_seconds" {
  description = "Age of the oldest queued message that raises the backlog alarm"
  type        = number
  default     = 900
}

variable "queues" {
  description = "Main queue names by logical queue"
  type        = map(string)
}

variable "service_names" {
  description = "ECS services that must always run at least one task"
  type        = list(string)
}

variable "tags" {
  description = "Tags applied to alarms"
  type        = map(string)
  default     = {}
}

variable "target_groups" {
  description = "Target group ARN suffixes by name (CloudWatch dimension)"
  type        = map(string)
}
