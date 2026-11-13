variable "name" {
  description = "Name prefix for the key alias"
  type        = string
}

variable "tags" {
  description = "Tags applied to the key"
  type        = map(string)
  default     = {}
}
