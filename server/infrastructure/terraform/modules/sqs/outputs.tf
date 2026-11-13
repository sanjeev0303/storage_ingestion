output "dead_letter_queue_names" {
  description = "Dead-letter queue names by logical queue, for alarms"
  value       = { for k, q in aws_sqs_queue.dead_letter : k => q.name }
}

output "queue_arns" {
  description = "ARNs of every queue and dead-letter queue, for IAM"
  value       = concat([for q in aws_sqs_queue.main : q.arn], [for q in aws_sqs_queue.dead_letter : q.arn])
}

output "queue_names" {
  description = "Main queue names by logical queue"
  value       = { for k, q in aws_sqs_queue.main : k => q.name }
}
