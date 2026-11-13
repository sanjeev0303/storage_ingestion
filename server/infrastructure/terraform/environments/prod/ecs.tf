# The platform's containers on ECS Fargate: the Go API, the Next.js
# console, the workers, and two one-off tasks (migrations, first admin).
# One server image carries every Go binary (infrastructure/docker/
# Dockerfile.server); each task picks its binary with entryPoint.

# ─── Images ───────────────────────────────────────────────────────────

# server and console are built here; clamav and busybox are mirrored from
# Docker Hub by scripts/build-and-push.sh, so tasks never pull from it.
resource "aws_ecr_repository" "main" {
  for_each = toset(["server", "console", "clamav", "busybox"])

  name                 = "${local.name}/${each.value}"
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
    kms_key         = module.kms.key_arn
  }
}

resource "aws_ecr_lifecycle_policy" "main" {
  for_each = aws_ecr_repository.main

  repository = each.value.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the 50 most recent images (rollback targets)"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 50
      }
      action = { type = "expire" }
    }]
  })
}

# ─── Cluster and roles ────────────────────────────────────────────────

resource "aws_ecs_cluster" "main" {
  name = local.name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

module "iam" {
  source = "../../modules/iam"

  name                           = local.name
  kms_key_arn                    = module.kms.key_arn
  bucket_arns                    = [module.quarantine_bucket.arn, module.serving_bucket.arn]
  queue_arns                     = module.queues.queue_arns
  allow_customer_role_assumption = var.byos_enabled
  secret_arns = concat(
    [aws_secretsmanager_secret.database_url.arn],
    [for s in aws_secretsmanager_secret.app : s.arn],
  )
}

# ─── Security groups ──────────────────────────────────────────────────

resource "aws_security_group" "api" {
  name        = "${local.name}-api"
  description = "API tasks: the load balancer and the console reach port 8080"
  vpc_id      = module.network.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "api_from_alb" {
  security_group_id            = aws_security_group.api.id
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = 8080
  to_port                      = 8080
  ip_protocol                  = "tcp"
  description                  = "API traffic from the load balancer"
}

resource "aws_vpc_security_group_ingress_rule" "api_from_console" {
  security_group_id            = aws_security_group.api.id
  referenced_security_group_id = aws_security_group.console.id
  from_port                    = 8080
  to_port                      = 8080
  ip_protocol                  = "tcp"
  description                  = "API traffic from the console server"
}

resource "aws_security_group" "console" {
  name        = "${local.name}-console"
  description = "Console tasks: the load balancer reaches port 3100"
  vpc_id      = module.network.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "console_from_alb" {
  security_group_id            = aws_security_group.console.id
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = 3100
  to_port                      = 3100
  ip_protocol                  = "tcp"
  description                  = "Console traffic from the load balancer"
}

# Workers and one-off tasks take no inbound traffic.
resource "aws_security_group" "workers" {
  name        = "${local.name}-workers"
  description = "Workers and one-off tasks: no inbound traffic"
  vpc_id      = module.network.vpc_id
}

# Tasks reach AWS APIs, customer storage, webhook receivers and ClamAV's
# signature mirror, so outbound is open (through NAT and VPC endpoints).
resource "aws_vpc_security_group_egress_rule" "tasks_out" {
  for_each = {
    api     = aws_security_group.api.id
    console = aws_security_group.console.id
    workers = aws_security_group.workers.id
  }

  security_group_id = each.value
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  description       = "Outbound from ${each.key} tasks"
}

# ─── Internal name of the API ─────────────────────────────────────────

resource "aws_service_discovery_private_dns_namespace" "main" {
  name = local.internal_domain
  vpc  = module.network.vpc_id
}

resource "aws_service_discovery_service" "api" {
  name = "api"

  dns_config {
    namespace_id   = aws_service_discovery_private_dns_namespace.main.id
    routing_policy = "MULTIVALUE"

    dns_records {
      type = "A"
      ttl  = 10
    }
  }
}

# ─── Container definitions ────────────────────────────────────────────

locals {
  server_env_list    = [for k in sort(keys(local.server_environment)) : { name = k, value = local.server_environment[k] }]
  server_secret_list = [for k in sort(keys(local.server_secrets)) : { name = k, valueFrom = local.server_secrets[k] }]

  # The root filesystem is read-only; scratch files go to the task's disk.
  tmp_mount = [{ sourceVolume = "tmp", containerPath = "/tmp", readOnly = false }]

  clamav_container = {
    name      = "clamav"
    image     = local.clamav_image
    essential = true
    healthCheck = {
      command     = ["CMD", "clamdcheck.sh"]
      interval    = 60
      timeout     = 10
      retries     = 3
      startPeriod = 300
    }
  }
}

# ─── API ──────────────────────────────────────────────────────────────

module "api" {
  source = "../../modules/ecs-service"

  name                 = "${local.name}-api"
  cluster_arn          = aws_ecs_cluster.main.arn
  cpu                  = 1024
  memory               = 2048
  autoscaling          = var.api_autoscaling
  subnet_ids           = module.network.private_subnet_ids
  security_group_ids   = [aws_security_group.api.id]
  execution_role_arn   = module.iam.execution_role_arn
  task_role_arn        = module.iam.server_task_role_arn
  kms_key_arn          = module.kms.key_arn
  log_retention_days   = var.log_retention_days
  service_registry_arn = aws_service_discovery_service.api.arn
  volumes              = ["tmp"]

  load_balancer = {
    target_group_arn = aws_lb_target_group.api.arn
    container_name   = "api"
    container_port   = 8080
  }

  containers = [{
    name                   = "api"
    image                  = local.server_image
    essential              = true
    entryPoint             = ["/usr/local/bin/api"]
    portMappings           = [{ containerPort = 8080, protocol = "tcp" }]
    environment            = local.server_env_list
    secrets                = local.server_secret_list
    readonlyRootFilesystem = true
    mountPoints            = local.tmp_mount
    stopTimeout            = 120
  }]

  depends_on = [aws_lb_listener.https]
}

# ─── Console ──────────────────────────────────────────────────────────

module "console" {
  source = "../../modules/ecs-service"

  name               = "${local.name}-console"
  cluster_arn        = aws_ecs_cluster.main.arn
  cpu                = 512
  memory             = 1024
  autoscaling        = var.console_autoscaling
  subnet_ids         = module.network.private_subnet_ids
  security_group_ids = [aws_security_group.console.id]
  execution_role_arn = module.iam.execution_role_arn
  task_role_arn      = module.iam.console_task_role_arn
  kms_key_arn        = module.kms.key_arn
  log_retention_days = var.log_retention_days

  load_balancer = {
    target_group_arn = aws_lb_target_group.console.arn
    container_name   = "console"
    container_port   = 3100
  }

  containers = [{
    name         = "console"
    image        = local.console_image
    essential    = true
    portMappings = [{ containerPort = 3100, protocol = "tcp" }]
    environment = [
      { name = "NODE_ENV", value = "production" },
      { name = "BACKEND_URL", value = local.api_internal_url },
      { name = "CONSOLE_ORIGIN", value = local.console_url },
      { name = "COOKIE_SECURE", value = "true" },
      # The load balancer appends one X-Forwarded-For entry.
      { name = "TRUSTED_PROXY_HOPS", value = "1" },
    ]
    stopTimeout = 60
  }]

  depends_on = [aws_lb_listener.https]
}

# ─── Workers ──────────────────────────────────────────────────────────

module "worker" {
  source   = "../../modules/ecs-service"
  for_each = local.workers

  name                  = "${local.name}-${each.key}"
  cluster_arn           = aws_ecs_cluster.main.arn
  cpu                   = each.value.cpu
  memory                = each.value.memory
  desired_count         = each.value.desired_count
  ephemeral_storage_gib = each.value.storage
  subnet_ids            = module.network.private_subnet_ids
  security_group_ids    = [aws_security_group.workers.id]
  execution_role_arn    = module.iam.execution_role_arn
  task_role_arn         = module.iam.server_task_role_arn
  kms_key_arn           = module.kms.key_arn
  log_retention_days    = var.log_retention_days
  volumes               = ["tmp"]

  containers = concat(
    [{
      name       = each.key
      image      = local.server_image
      essential  = true
      entryPoint = ["/usr/local/bin/${each.key}"]
      # The processing worker scans with the ClamAV sidecar on localhost.
      environment = each.value.clamav ? concat(local.server_env_list, [
        { name = "SCANNER", value = "clamav" },
        { name = "CLAMAV_ADDRESS", value = "127.0.0.1:3310" },
      ]) : local.server_env_list
      secrets                = local.server_secret_list
      readonlyRootFilesystem = true
      mountPoints            = local.tmp_mount
      stopTimeout            = 120
      dependsOn              = each.value.clamav ? [{ containerName = "clamav", condition = "START" }] : []
    }],
    each.value.clamav ? [local.clamav_container] : [],
  )
}

# ─── One-off tasks ────────────────────────────────────────────────────

# Database migrations: scripts/migrate.sh runs this before services move to
# a new image. It may run ahead of the services (migration_image_tag).
module "migrate" {
  source = "../../modules/ecs-service"

  name               = "${local.name}-migrate"
  create_service     = false
  cluster_arn        = aws_ecs_cluster.main.arn
  cpu                = 512
  memory             = 1024
  subnet_ids         = module.network.private_subnet_ids
  security_group_ids = [aws_security_group.workers.id]
  execution_role_arn = module.iam.execution_role_arn
  task_role_arn      = module.iam.server_task_role_arn
  kms_key_arn        = module.kms.key_arn
  log_retention_days = 365

  containers = [{
    name                   = "migrate"
    image                  = local.migration_image
    essential              = true
    entryPoint             = ["/usr/local/bin/migrate"]
    command                = ["up"]
    secrets                = [{ name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.database_url.arn }]
    readonlyRootFilesystem = true
  }]
}

# The first platform administrator. identity-admin prints a generated
# password to stdout, which would land in the logs, so the password comes
# from Secrets Manager instead: an init container writes it to a task-scoped
# volume, and identity-admin reads it with -password-file.
module "admin" {
  source = "../../modules/ecs-service"

  name               = "${local.name}-admin"
  create_service     = false
  cluster_arn        = aws_ecs_cluster.main.arn
  cpu                = 512
  memory             = 1024
  subnet_ids         = module.network.private_subnet_ids
  security_group_ids = [aws_security_group.workers.id]
  execution_role_arn = module.iam.execution_role_arn
  task_role_arn      = module.iam.server_task_role_arn
  kms_key_arn        = module.kms.key_arn
  log_retention_days = 365
  volumes            = ["bootstrap"]

  containers = [
    {
      name      = "write-password"
      image     = local.busybox_image
      essential = false
      command = [
        "sh", "-c",
        "printf %s \"$BOOTSTRAP_PASSWORD\" > /bootstrap/password && chown 65532:65532 /bootstrap/password && chmod 400 /bootstrap/password",
      ]
      secrets     = [{ name = "BOOTSTRAP_PASSWORD", valueFrom = aws_secretsmanager_secret.app["admin-bootstrap-password"].arn }]
      mountPoints = [{ sourceVolume = "bootstrap", containerPath = "/bootstrap", readOnly = false }]
    },
    {
      name                   = "identity-admin"
      image                  = local.server_image
      essential              = true
      entryPoint             = ["/usr/local/bin/identity-admin"]
      command                = ["bootstrap", "-email", var.admin_email, "-password-file", "/bootstrap/password"]
      environment            = local.server_env_list
      secrets                = local.server_secret_list
      readonlyRootFilesystem = true
      mountPoints            = [{ sourceVolume = "bootstrap", containerPath = "/bootstrap", readOnly = true }]
      dependsOn              = [{ containerName = "write-password", condition = "SUCCESS" }]
    },
  ]
}
