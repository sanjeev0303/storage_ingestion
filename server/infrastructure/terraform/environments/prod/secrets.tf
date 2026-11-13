# Secrets the tasks receive as environment variables at start.
#
# DATABASE_URL is written here, write-only, from the same ephemeral password
# the database instance gets: neither value is ever stored in the state.
#
# The others are created empty and filled once by scripts/init-secrets.sh,
# which generates them with openssl and never prints them, so their values
# never pass through Terraform at all. Tasks fail to start until they exist.

resource "aws_secretsmanager_secret" "database_url" {
  name                    = "${local.name}/database-url"
  description             = "DATABASE_URL of the platform (master user, TLS required)"
  kms_key_id              = module.kms.key_arn
  recovery_window_in_days = 30
}

resource "aws_secretsmanager_secret_version" "database_url" {
  secret_id                = aws_secretsmanager_secret.database_url.id
  secret_string_wo         = "postgres://${module.database.master_username}:${ephemeral.random_password.db_master.result}@${module.database.address}:${module.database.port}/${module.database.database_name}?sslmode=require"
  secret_string_wo_version = var.db_password_version
}

locals {
  app_secrets = {
    "api-token-pepper"         = "API_TOKEN_PEPPER: keys the HMAC API tokens are stored as (base64 of 32 random bytes)"
    "storage-credential-keys"  = "STORAGE_CREDENTIAL_KEYS: version:base64 keys that seal stored credentials"
    "certificate-signing-key"  = "CERTIFICATE_SIGNING_KEY: base64 Ed25519 seed that signs disposition certificates"
    "admin-bootstrap-password" = "Password of the first platform administrator (scripts/bootstrap-admin.sh)"
  }
}

resource "aws_secretsmanager_secret" "app" {
  for_each = local.app_secrets

  name                    = "${local.name}/${each.key}"
  description             = each.value
  kms_key_id              = module.kms.key_arn
  recovery_window_in_days = 30
}
