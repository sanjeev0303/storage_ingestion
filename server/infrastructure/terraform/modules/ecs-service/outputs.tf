output "log_group_name" {
  description = "Log group of the task's containers"
  value       = aws_cloudwatch_log_group.main.name
}

output "service_name" {
  description = "Name of the ECS service, or null for a one-off task"
  value       = one(concat(aws_ecs_service.fixed[*].name, aws_ecs_service.scaled[*].name))
}

output "task_definition_arn" {
  description = "ARN of the current task definition revision"
  value       = aws_ecs_task_definition.main.arn
}

output "task_family" {
  description = "Task definition family, for RunTask"
  value       = aws_ecs_task_definition.main.family
}
