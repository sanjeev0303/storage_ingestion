# The event queues, exactly as the services expect them
# (internal/infrastructure/queue/sqs): for each logical queue a FIFO queue
# <prefix><name>.fifo and its dead-letter queue <prefix><name>-dlq.fifo,
# joined by a redrive policy. The services run with SQS_CREATE_QUEUES=false
# and only look these up.

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

resource "aws_sqs_queue" "dead_letter" {
  for_each = toset(var.queues)

  name       = "${var.prefix}${each.value}-dlq.fifo"
  fifo_queue = true
  # The longest SQS allows, so nothing is lost before reconciliation drains it.
  message_retention_seconds         = 1209600
  kms_master_key_id                 = var.kms_key_arn
  kms_data_key_reuse_period_seconds = 300
  tags                              = var.tags
}

resource "aws_sqs_queue" "main" {
  for_each = toset(var.queues)

  name                              = "${var.prefix}${each.value}.fifo"
  fifo_queue                        = true
  message_retention_seconds         = var.message_retention_seconds
  visibility_timeout_seconds        = var.visibility_timeout_seconds
  receive_wait_time_seconds         = 10
  kms_master_key_id                 = var.kms_key_arn
  kms_data_key_reuse_period_seconds = 300

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dead_letter[each.value].arn
    maxReceiveCount     = var.max_receive_count
  })

  tags = var.tags
}

# A dead-letter queue takes messages from its own queue only.
resource "aws_sqs_queue_redrive_allow_policy" "dead_letter" {
  for_each = toset(var.queues)

  queue_url = aws_sqs_queue.dead_letter[each.value].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.main[each.value].arn]
  })
}
