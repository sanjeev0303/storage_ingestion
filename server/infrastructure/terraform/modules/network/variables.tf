variable "availability_zones" {
  description = "Availability zones to spread subnets across (two or three)"
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) >= 2 && length(var.availability_zones) <= 4
    error_message = "Use two to four availability zones."
  }
}

variable "cidr_block" {
  description = "VPC CIDR block; a /16 leaves room for /20 subnets in every tier"
  type        = string
  default     = "10.40.0.0/16"

  validation {
    condition     = can(cidrhost(var.cidr_block, 0)) && tonumber(split("/", var.cidr_block)[1]) <= 16
    error_message = "cidr_block must be a valid CIDR of /16 or larger."
  }
}

variable "flow_log_retention_days" {
  description = "Days to keep VPC flow logs"
  type        = number
  default     = 90
}

variable "interface_endpoints" {
  description = "Interface VPC endpoints to create (service suffixes such as ecr.api); empty uses NAT for everything but S3"
  type        = list(string)
  default     = ["ecr.api", "ecr.dkr", "logs", "secretsmanager", "sqs", "sts", "kms"]
}

variable "kms_key_arn" {
  description = "KMS key that encrypts the flow log group"
  type        = string
}

variable "name" {
  description = "Name prefix for network resources"
  type        = string
}

variable "single_nat_gateway" {
  description = "One NAT gateway for all zones (cheaper, but a zone outage cuts egress); false gives one per zone"
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags applied to network resources"
  type        = map(string)
  default     = {}
}
