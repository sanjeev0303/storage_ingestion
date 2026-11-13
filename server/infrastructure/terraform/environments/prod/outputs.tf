output "account_id" {
  description = "Production AWS account"
  value       = data.aws_caller_identity.current.account_id
}

output "admin_task_family" {
  description = "Task family that makes the first administrator (scripts/bootstrap-admin.sh)"
  value       = module.admin.task_family
}

output "alarm_topic_arn" {
  description = "SNS topic alarms publish to"
  value       = module.monitoring.alarm_topic_arn
}

output "api_url" {
  description = "Public URL of the API (SDKs, CLI, Git LFS)"
  value       = "https://${local.api_host}"
}

output "app_secret_ids" {
  description = "Secrets scripts/init-secrets.sh fills, by name"
  value       = { for k, s in aws_secretsmanager_secret.app : k => s.id }
}

output "buckets" {
  description = "Platform buckets"
  value = {
    quarantine = module.quarantine_bucket.name
    serving    = module.serving_bucket.name
  }
}

output "cluster_name" {
  description = "ECS cluster"
  value       = aws_ecs_cluster.main.name
}

output "console_url" {
  description = "Public URL of the console"
  value       = local.console_url
}

output "ecr_repositories" {
  description = "ECR repository URLs by image name"
  value       = { for k, r in aws_ecr_repository.main : k => r.repository_url }
}

output "image_tag" {
  description = "Image tag the services run now (the rollback target of the next deploy)"
  value       = var.image_tag
}

output "mirror_image_tags" {
  description = "Tags scripts/build-and-push.sh mirrors from Docker Hub"
  value = {
    clamav  = var.clamav_image_tag
    busybox = var.busybox_image_tag
  }
}

output "migrate_task_family" {
  description = "Task family that applies database migrations (scripts/migrate.sh)"
  value       = module.migrate.task_family
}

output "nat_public_ips" {
  description = "Addresses outbound traffic comes from (for webhook receivers' allow-lists)"
  value       = module.network.nat_public_ips
}

output "one_off_network" {
  description = "Network settings for one-off tasks run with RunTask"
  value = {
    subnets         = module.network.private_subnet_ids
    security_groups = [aws_security_group.workers.id]
  }
}

output "region" {
  description = "AWS region of the stack"
  value       = var.region
}

output "service_names" {
  description = "ECS services, for waiting until a deployment is stable"
  value = compact(concat(
    [module.api.service_name, module.console.service_name],
    [for w in module.worker : w.service_name],
  ))
}
