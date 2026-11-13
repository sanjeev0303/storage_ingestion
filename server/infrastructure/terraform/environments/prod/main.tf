# Production foundation: the platform key, the network, the buckets, the
# event queues and the database. The services that use them are in ecs.tf,
# the load balancer and DNS in alb.tf, secrets in secrets.tf.

data "aws_caller_identity" "current" {}

module "kms" {
  source = "../../modules/kms"

  name = local.name
}

module "network" {
  source = "../../modules/network"

  name                = local.name
  cidr_block          = var.vpc_cidr
  availability_zones  = var.availability_zones
  single_nat_gateway  = var.single_nat_gateway
  interface_endpoints = var.interface_endpoints
  kms_key_arn         = module.kms.key_arn
}

# ─── Buckets ──────────────────────────────────────────────────────────

# Uploads land here (S3_BUCKET): staging, quarantine and ingestion
# manifests. Browsers PUT to presigned URLs from the console's origin.
module "quarantine_bucket" {
  source = "../../modules/s3-bucket"

  name                 = "${local.name}-quarantine"
  kms_key_arn          = module.kms.key_arn
  cors_allowed_origins = [local.console_url]
  access_log_bucket    = var.alb_access_logs ? aws_s3_bucket.logs[0].id : ""
}

# Clean, promoted objects (PROMOTION_BUCKET), apart from the quarantine
# bucket so a quarantined key can never be served. Versioned, as dataset
# snapshots pin versions, and with Object Lock so legal holds bind the
# bucket as well as the platform.
module "serving_bucket" {
  source = "../../modules/s3-bucket"

  name              = "${local.name}-serving"
  kms_key_arn       = module.kms.key_arn
  versioning        = true
  object_lock       = true
  access_log_bucket = var.alb_access_logs ? aws_s3_bucket.logs[0].id : ""
}

# Load balancer and S3 access logs. Both deliveries need SSE-S3, not KMS.
resource "aws_s3_bucket" "logs" {
  count = var.alb_access_logs ? 1 : 0

  bucket_prefix = "${local.name}-logs-"

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "logs" {
  count = var.alb_access_logs ? 1 : 0

  bucket = aws_s3_bucket.logs[0].id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "logs" {
  count = var.alb_access_logs ? 1 : 0

  bucket                  = aws_s3_bucket.logs[0].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "logs" {
  count = var.alb_access_logs ? 1 : 0

  bucket = aws_s3_bucket.logs[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "logs" {
  count = var.alb_access_logs ? 1 : 0

  bucket = aws_s3_bucket.logs[0].id

  rule {
    id     = "expire-logs"
    status = "Enabled"

    filter {}

    expiration {
      days = 365
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# The account Elastic Load Balancing delivers logs from in this region.
data "aws_elb_service_account" "main" {}

data "aws_iam_policy_document" "logs" {
  count = var.alb_access_logs ? 1 : 0

  statement {
    sid       = "LoadBalancerLogs"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.logs[0].arn}/alb/AWSLogs/${data.aws_caller_identity.current.account_id}/*"]

    principals {
      type        = "AWS"
      identifiers = [data.aws_elb_service_account.main.arn]
    }
  }

  statement {
    sid       = "S3ServerAccessLogs"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.logs[0].arn}/*"]

    principals {
      type        = "Service"
      identifiers = ["logging.s3.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.logs[0].arn,
      "${aws_s3_bucket.logs[0].arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "logs" {
  count = var.alb_access_logs ? 1 : 0

  bucket = aws_s3_bucket.logs[0].id
  policy = data.aws_iam_policy_document.logs[0].json

  depends_on = [aws_s3_bucket_public_access_block.logs]
}

# ─── Event queues ─────────────────────────────────────────────────────

module "queues" {
  source = "../../modules/sqs"

  prefix      = local.queue_prefix
  kms_key_arn = module.kms.key_arn
}

# ─── Database ─────────────────────────────────────────────────────────

# Made fresh on every run but applied only when db_password_version
# changes, and then to the instance and the DATABASE_URL secret together.
# Letters and digits only, so it needs no escaping inside the URL.
ephemeral "random_password" "db_master" {
  length  = 40
  special = false
}

module "database" {
  source = "../../modules/rds"

  name                    = local.name
  vpc_id                  = module.network.vpc_id
  subnet_ids              = module.network.database_subnet_ids
  instance_class          = var.db_instance_class
  allocated_storage_gb    = var.db_allocated_storage_gb
  multi_az                = var.db_multi_az
  backup_retention_days   = var.db_backup_retention_days
  kms_key_arn             = module.kms.key_arn
  master_password         = ephemeral.random_password.db_master.result
  master_password_version = var.db_password_version

  allowed_security_groups = {
    api     = aws_security_group.api.id
    workers = aws_security_group.workers.id
  }
}
