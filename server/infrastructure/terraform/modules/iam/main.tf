# Roles for the ECS tasks:
#   execution - what ECS itself needs to start a task: pull images, write
#               logs, read the task's secrets
#   server    - what the Go API and workers do at run time: the two platform
#               buckets, the event queues and the platform key
#   console   - the Next.js console calls only the API, so it gets no AWS
#               permissions at all

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

data "aws_partition" "current" {}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# ─── Execution role ───────────────────────────────────────────────────

resource "aws_iam_role" "execution" {
  name               = "${var.name}-ecs-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  tags               = var.tags
}

resource "aws_iam_role_policy_attachment" "execution_managed" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "execution_secrets" {
  statement {
    sid       = "ReadTaskSecrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = var.secret_arns
  }

  statement {
    sid       = "DecryptTaskSecrets"
    actions   = ["kms:Decrypt"]
    resources = [var.kms_key_arn]
  }
}

resource "aws_iam_role_policy" "execution_secrets" {
  name   = "read-task-secrets"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution_secrets.json
}

# ─── Server task role ─────────────────────────────────────────────────

resource "aws_iam_role" "server" {
  name               = "${var.name}-server-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  tags               = var.tags
}

data "aws_iam_policy_document" "server" {
  statement {
    sid = "PlatformBuckets"
    actions = [
      "s3:GetObject",
      "s3:GetObjectVersion",
      "s3:GetObjectAttributes",
      "s3:GetObjectTagging",
      "s3:PutObject",
      "s3:PutObjectTagging",
      "s3:DeleteObject",
      "s3:DeleteObjectVersion",
      "s3:AbortMultipartUpload",
      "s3:ListMultipartUploadParts",
      "s3:GetObjectLegalHold",
      "s3:PutObjectLegalHold",
      "s3:GetObjectRetention",
      "s3:PutObjectRetention",
    ]
    resources = [for arn in var.bucket_arns : "${arn}/*"]
  }

  statement {
    sid = "PlatformBucketListing"
    actions = [
      "s3:ListBucket",
      "s3:ListBucketVersions",
      "s3:ListBucketMultipartUploads",
      "s3:GetBucketLocation",
      "s3:GetBucketVersioning",
      "s3:GetBucketObjectLockConfiguration",
      "s3:GetLifecycleConfiguration",
      "s3:GetEncryptionConfiguration",
      "s3:GetBucketPublicAccessBlock",
      "s3:GetBucketPolicyStatus",
      "s3:GetBucketPolicy",
      "s3:GetBucketOwnershipControls",
      "s3:GetBucketAcl",
    ]
    resources = var.bucket_arns
  }

  statement {
    sid = "EventQueues"
    actions = [
      "sqs:SendMessage",
      "sqs:ReceiveMessage",
      "sqs:DeleteMessage",
      "sqs:ChangeMessageVisibility",
      "sqs:GetQueueUrl",
      "sqs:GetQueueAttributes",
    ]
    resources = var.queue_arns
  }

  statement {
    sid = "PlatformKey"
    actions = [
      "kms:Decrypt",
      "kms:Encrypt",
      "kms:GenerateDataKey",
      "kms:GenerateDataKeyWithoutPlaintext",
      "kms:DescribeKey",
    ]
    resources = [var.kms_key_arn]
  }

  # Bring-your-own-storage connections of type ASSUME_ROLE: the platform
  # assumes a role in the customer's account, which the customer's trust
  # policy binds to this role and a platform-generated external ID.
  dynamic "statement" {
    for_each = var.allow_customer_role_assumption ? [1] : []

    content {
      sid       = "CustomerStorageRoles"
      actions   = ["sts:AssumeRole"]
      resources = ["arn:${data.aws_partition.current.partition}:iam::*:role/*"]

      condition {
        test     = "StringLike"
        variable = "sts:ExternalId"
        values   = ["*"]
      }
    }
  }
}

resource "aws_iam_role_policy" "server" {
  name   = "platform-runtime"
  role   = aws_iam_role.server.id
  policy = data.aws_iam_policy_document.server.json
}

# ─── Console task role ────────────────────────────────────────────────

resource "aws_iam_role" "console" {
  name               = "${var.name}-console-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  tags               = var.tags
}
