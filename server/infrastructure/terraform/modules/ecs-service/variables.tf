variable "autoscaling" {
  description = "CPU target tracking (min and max tasks, target percent); null keeps desired_count fixed"
  type = object({
    min        = number
    max        = number
    cpu_target = number
  })
  default = null
}

variable "cluster_arn" {
  description = "ECS cluster the service runs in"
  type        = string
}

variable "containers" {
  description = "ECS container definitions (logConfiguration is added here)"
  type        = any
}

variable "cpu" {
  description = "Task CPU units (1024 = one vCPU)"
  type        = number
}

variable "create_service" {
  description = "false registers only the task definition, for one-off tasks run with RunTask"
  type        = bool
  default     = true
}

variable "desired_count" {
  description = "Tasks to run when autoscaling is off (autoscaled services start at autoscaling.min)"
  type        = number
  default     = 1
}

variable "ephemeral_storage_gib" {
  description = "Task scratch disk in GiB (21-200); workers spool objects here"
  type        = number
  default     = 21

  validation {
    condition     = var.ephemeral_storage_gib >= 21 && var.ephemeral_storage_gib <= 200
    error_message = "Fargate ephemeral storage is 21 to 200 GiB."
  }
}

variable "execution_role_arn" {
  description = "ECS task execution role"
  type        = string
}

variable "health_check_grace_seconds" {
  description = "Seconds before load balancer health checks count for a new task"
  type        = number
  default     = 60
}

variable "kms_key_arn" {
  description = "KMS key that encrypts the log group"
  type        = string
}

variable "load_balancer" {
  description = "Target group to register the service's tasks with; null for workers"
  type = object({
    target_group_arn = string
    container_name   = string
    container_port   = number
  })
  default = null
}

variable "log_retention_days" {
  description = "Days to keep the task's logs"
  type        = number
  default     = 90
}

variable "memory" {
  description = "Task memory in MiB"
  type        = number
}

variable "name" {
  description = "Name of the task family and service"
  type        = string
}

variable "security_group_ids" {
  description = "Security groups of the tasks"
  type        = list(string)
}

variable "service_registry_arn" {
  description = "Cloud Map service to register tasks in, for internal addressing; null for none"
  type        = string
  default     = null
}

variable "subnet_ids" {
  description = "Private subnets the tasks run in"
  type        = list(string)
}

variable "tags" {
  description = "Tags applied to the task definition, service and logs"
  type        = map(string)
  default     = {}
}

variable "task_role_arn" {
  description = "Role the containers run as"
  type        = string
}

variable "volumes" {
  description = "Names of task-scoped scratch volumes shared between containers"
  type        = list(string)
  default     = []
}
