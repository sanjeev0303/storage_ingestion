output "console_task_role_arn" {
  description = "Task role of the console (no AWS permissions)"
  value       = aws_iam_role.console.arn
}

output "execution_role_arn" {
  description = "ECS task execution role"
  value       = aws_iam_role.execution.arn
}

output "server_task_role_arn" {
  description = "Task role of the Go API and workers"
  value       = aws_iam_role.server.arn
}
