provider "aws" {
  region = var.region

  # Refuse to touch any account but production's.
  allowed_account_ids = [var.aws_account_id]

  default_tags {
    tags = local.tags
  }
}
