# A private platform bucket that passes the platform's own posture checks
# (internal/infrastructure/storage/s3/posture.go): all public access
# blocked, ACLs disabled, SSE-KMS by default, SSE-C writes denied, TLS only,
# and a single lifecycle rule that aborts incomplete multipart uploads.
# The platform decides when objects expire; no rule here may expire or
# archive them.

terraform {
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
}

# A random suffix keeps the name from being guessed (posture: guessable_name).
resource "random_id" "suffix" {
  byte_length = 4
}

resource "aws_s3_bucket" "main" {
  bucket              = "${var.name}-${random_id.suffix.hex}"
  object_lock_enabled = var.object_lock
  tags                = var.tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "main" {
  bucket = aws_s3_bucket.main.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "main" {
  bucket = aws_s3_bucket.main.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "main" {
  bucket = aws_s3_bucket.main.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.kms_key_arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_versioning" "main" {
  count = var.versioning || var.object_lock ? 1 : 0

  bucket = aws_s3_bucket.main.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "main" {
  bucket = aws_s3_bucket.main.id

  rule {
    id     = "abort-incomplete-multipart-uploads"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = var.abort_incomplete_upload_days
    }
  }
}

resource "aws_s3_bucket_cors_configuration" "main" {
  count = length(var.cors_allowed_origins) > 0 ? 1 : 0

  bucket = aws_s3_bucket.main.id

  # Browsers upload straight to presigned URLs; multipart needs the ETag back.
  cors_rule {
    allowed_methods = ["GET", "HEAD", "PUT"]
    allowed_origins = var.cors_allowed_origins
    allowed_headers = ["*"]
    expose_headers  = ["ETag", "x-amz-checksum-sha256", "x-amz-version-id"]
    max_age_seconds = 3600
  }
}

resource "aws_s3_bucket_logging" "main" {
  count = var.access_log_bucket != "" ? 1 : 0

  bucket        = aws_s3_bucket.main.id
  target_bucket = var.access_log_bucket
  target_prefix = "${aws_s3_bucket.main.id}/"
}

data "aws_iam_policy_document" "main" {
  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.main.arn,
      "${aws_s3_bucket.main.arn}/*",
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

  # posture: sse_c_denied. Objects encrypted with a caller's own key could
  # not be read, scanned or delivered by the platform.
  statement {
    sid       = "DenySSEC"
    effect    = "Deny"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.main.arn}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Null"
      variable = "s3:x-amz-server-side-encryption-customer-algorithm"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "main" {
  bucket = aws_s3_bucket.main.id
  policy = data.aws_iam_policy_document.main.json

  depends_on = [aws_s3_bucket_public_access_block.main]
}
