locals {
  name         = "${var.project}-prod"
  console_host = "${var.console_subdomain}.${var.domain_name}"
  api_host     = "${var.api_subdomain}.${var.domain_name}"
  console_url  = "https://${local.console_host}"

  tags = {
    Project     = var.project
    Environment = "prod"
    ManagedBy   = "Terraform"
  }

  server_image    = "${aws_ecr_repository.main["server"].repository_url}:${var.image_tag}"
  migration_image = "${aws_ecr_repository.main["server"].repository_url}:${coalesce(var.migration_image_tag, var.image_tag)}"
  console_image   = "${aws_ecr_repository.main["console"].repository_url}:${var.image_tag}"
  clamav_image    = "${aws_ecr_repository.main["clamav"].repository_url}:${var.clamav_image_tag}"
  busybox_image   = "${aws_ecr_repository.main["busybox"].repository_url}:${var.busybox_image_tag}"

  # The API and the console reach each other inside the VPC through Cloud
  # Map, so nothing appends an X-Forwarded-For entry between them: the API
  # sees the one address the console sends, and SDK clients come through
  # the load balancer, which appends theirs last.
  internal_domain  = "storage.internal"
  api_internal_url = "http://api.${local.internal_domain}:8080"

  # Settings every Go process reads (server/.env.example has the rest at
  # their defaults; change them with server_environment_overrides).
  server_environment = merge({
    APP_ENV                        = "production"
    HTTP_PORT                      = "8080"
    S3_REGION                      = var.region
    S3_BUCKET                      = module.quarantine_bucket.name
    PROMOTION_BUCKET               = module.serving_bucket.name
    PROMOTION_PREFIX               = "assets"
    EVENT_QUEUE                    = "sqs"
    SQS_REGION                     = var.region
    SQS_QUEUE_PREFIX               = local.queue_prefix
    SQS_CREATE_QUEUES              = "false"
    SQS_MAX_RECEIVE_COUNT          = "20"
    BYOS_ENABLED                   = tostring(var.byos_enabled)
    STORAGE_CREDENTIAL_KEY_VERSION = "1"
    AUTH_TRUST_PROXY_HEADERS       = "true"
    AUTH_SELF_REGISTRATION         = tostring(var.self_registration)
    AUTH_SSO_REDIRECT_URIS         = "${local.console_url}/api/session/sso/callback"
    AUTH_SAML_ACS_URL              = "${local.console_url}/api/session/saml/acs"
    CERTIFICATE_KEY_ID             = var.certificate_key_id
    CONTENT_CHECKS_ENABLED         = "true"
    SCAN_UNAVAILABLE_POLICY        = "hold"
  }, var.server_environment_overrides)

  queue_prefix = "${var.project}-prod-"

  # Secrets every Go process receives, by environment variable name.
  server_secrets = {
    DATABASE_URL            = aws_secretsmanager_secret.database_url.arn
    API_TOKEN_PEPPER        = aws_secretsmanager_secret.app["api-token-pepper"].arn
    STORAGE_CREDENTIAL_KEYS = aws_secretsmanager_secret.app["storage-credential-keys"].arn
    CERTIFICATE_SIGNING_KEY = aws_secretsmanager_secret.app["certificate-signing-key"].arn
  }

  # Long-running workers (cmd/<name>). All of them hold leases in the
  # database, so more than one task of each is safe.
  worker_defaults = {
    "upload-worker"         = { cpu = 512, memory = 1024, desired_count = 2, storage = 21, clamav = false }
    "processing-worker"     = { cpu = 2048, memory = 8192, desired_count = 2, storage = 100, clamav = true }
    "outbox-worker"         = { cpu = 256, memory = 512, desired_count = 2, storage = 21, clamav = false }
    "dataset-worker"        = { cpu = 1024, memory = 4096, desired_count = 1, storage = 50, clamav = false }
    "cleanup-worker"        = { cpu = 512, memory = 1024, desired_count = 1, storage = 21, clamav = false }
    "compaction-worker"     = { cpu = 256, memory = 512, desired_count = 1, storage = 21, clamav = false }
    "reconciliation-worker" = { cpu = 512, memory = 1024, desired_count = 1, storage = 21, clamav = false }
    "pack-worker"           = { cpu = 1024, memory = 4096, desired_count = 1, storage = 50, clamav = false }
    "webhook-worker"        = { cpu = 256, memory = 512, desired_count = 1, storage = 21, clamav = false }
  }

  workers = {
    for name, w in local.worker_defaults : name => merge(w, {
      cpu           = coalesce(try(var.worker_overrides[name].cpu, null), w.cpu)
      memory        = coalesce(try(var.worker_overrides[name].memory, null), w.memory)
      desired_count = coalesce(try(var.worker_overrides[name].desired_count, null), w.desired_count)
    })
  }
}
