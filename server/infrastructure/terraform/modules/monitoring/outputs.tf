output "alarm_topic_arn" {
  description = "SNS topic alarms publish to"
  value       = aws_sns_topic.alarms.arn
}
