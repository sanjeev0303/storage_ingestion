terraform {
  # 1.11 for write-only attributes and ephemeral values (the database
  # password never reaches the state), S3-native state locking from 1.10.
  required_version = ">= 1.11"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }

  # Partial configuration: bucket, region and kms_key_id come from
  # backend.hcl (see backend.hcl.example and scripts/bootstrap-state.sh).
  backend "s3" {
    key          = "object-storage/prod/terraform.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}
