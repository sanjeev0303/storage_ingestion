# Production infrastructure

Terraform for the platform's production environment on AWS, the container images, and the scripts that deploy it. Nothing here has been applied yet.

## What it creates

| Layer | Resources |
|---|---|
| Network | VPC over 2–4 zones with public, private and database subnets; one NAT gateway per zone; an S3 gateway endpoint; interface endpoints for ECR, Logs, Secrets Manager, SQS, STS and KMS; flow logs of rejected traffic |
| Edge | Public ALB (TLS 1.3/1.2, invalid headers dropped, access logs); ACM certificate and Route 53 records for `console.<domain>` and `api.<domain>`; HTTP redirects to HTTPS; unknown hosts get 404 |
| Compute | ECS Fargate cluster (Container Insights) running the API (2–10 tasks, autoscaled), the console (2–6), and nine workers. The processing worker has a ClamAV sidecar. Migrations and the first-admin bootstrap are one-off tasks |
| Data | PostgreSQL 17 on RDS (Multi-AZ, encrypted, TLS required, 14-day point-in-time recovery, deletion protection); quarantine bucket (uploads); serving bucket (versioned, Object Lock); five FIFO SQS queues with dead-letter queues |
| Security | One KMS key for data at rest; Secrets Manager for `DATABASE_URL` and the generated secrets; least-privilege task roles (the console has none); security groups allow only ALB → tasks, console → API, tasks → database |
| Operations | Alarms to SNS email: ALB 5xx, unhealthy targets, database CPU, storage and memory, dead-letter messages, queue backlog, stopped services |

The console reaches the API inside the VPC through Cloud Map (`api.storage.internal`), so the API sees the caller address the console sends. SDK clients come through the ALB, which appends theirs. That is why the API runs with `AUTH_TRUST_PROXY_HEADERS=true` and the console with `TRUSTED_PROXY_HOPS=1`.

## Layout

```
infrastructure/
  terraform/bootstrap/          state bucket (once per account, local state)
  terraform/modules/            kms, network, rds, sqs, s3-bucket, iam, ecs-service, monitoring
  terraform/environments/prod/  the production stack
  docker/                       Dockerfile.server (every Go binary), Dockerfile.console
  scripts/                      deployment scripts (bash; Git Bash works on Windows)
```

The `s3-inventory`, `s3-lifecycle`, `s3-replication`, `s3-quarantine`, `s3-serving` and `cloudfront` module folders are earlier placeholders and stay empty. `modules/s3-bucket` serves both buckets, and the CDN is not set up (see "Not covered").

## Requirements

- Terraform ≥ 1.11, AWS CLI v2, Docker with BuildKit, `jq`, `curl`, `openssl`, `git`. Go and npm are also needed unless you pass `--skip-tests`.
- AWS credentials for the production account with administrator rights for the first deployment.
- A Route 53 hosted zone for your domain.

## First deployment

```bash
cd server/infrastructure

# 1. State bucket, once per account. Writes terraform/environments/prod/backend.hcl.
scripts/bootstrap-state.sh <globally-unique-state-bucket> <region>

# 2. Settings: copy and fill in (account, region, zones, domain, zone ID, emails).
cp terraform/environments/prod/terraform.tfvars.example terraform/environments/prod/terraform.tfvars

# 3. Deploy the current commit. It creates the network, database, repositories and
#    secrets, generates the secrets, builds and pushes the images, migrates, then
#    creates the services. Each apply shows its plan and asks first.
scripts/deploy.sh

# 4. The first platform administrator (admin_email); the password stays in Secrets Manager.
scripts/bootstrap-admin.sh
```

The database takes 15–25 minutes to create. Confirm the alarm subscription emails AWS sends.

## Deploying a change

Commit, then:

```bash
scripts/deploy.sh                 # tests, build, migrate, roll out, smoke test
scripts/deploy.sh --plan-only     # what would change, nothing applied
scripts/deploy.sh --yes           # CI: no prompts (destroying plans still stop)
```

Migrations run before any service moves to the new image, so they must leave the schema usable by the previous release (add first, remove one release later). ECS replaces tasks one at a time and returns to the previous task definition by itself if new tasks keep failing (the deployment circuit breaker).

## Rolling back

```bash
scripts/rollback.sh --list        # recent image tags
scripts/rollback.sh <tag>         # every service back to <tag>, then the smoke test
```

Rollback moves code only, never the schema. Undoing a migration (`migrate down N`, from a one-off task) is a deliberate operator decision.

## Secrets

| Secret | Set by | Changing it |
|---|---|---|
| `database-url` | Terraform, from a write-only password never stored in state | Bump `db_password_version` and deploy; the instance and the secret change together |
| `api-token-pepper` | `init-secrets.sh` | Invalidates every API token; avoid |
| `storage-credential-keys` | `init-secrets.sh` | Add a new `version:key`, raise `STORAGE_CREDENTIAL_KEY_VERSION`, deploy, run `credential-admin reseal` (docs/storage-connections.md) |
| `certificate-signing-key` | `init-secrets.sh` | New key plus new `certificate_key_id`; old certificates keep verifying against the old public key |
| `admin-bootstrap-password` | `init-secrets.sh` | Used once; change the password after the first sign-in |

`init-secrets.sh` never overwrites a secret that has a value and never prints one.

## Operating notes

- **Webhook receivers:** outbound traffic leaves from the NAT addresses (`terraform output nat_public_ips`), for receivers' allow-lists.
- **Dead letters:** alarms point at `event-admin` and docs/events.md. Run admin commands as one-off tasks from the server image, with entryPoint `/usr/local/bin/<command>`.
- **Settings:** tuning comes from `server_environment_overrides`, sizing from `worker_overrides`.
- **Configuration drift:** keep changes in Terraform. Plans that delete resources stop the scripts unless `ALLOW_DESTROY=1`.

## Not covered, and what to decide

- **Staging:** no staging environment yet. `environments/dev` and `environments/staging` are still placeholders; a staging copy of `prod` with smaller sizes is the next step before relying on these scripts.
- **CDN delivery:** `DELIVERY_CDN_ENABLED` needs the CloudFront private key as a file, which the distroless image cannot write. Delivery stays in storage mode (presigned S3 URLs) until a key-file init container or a code change reads the key from the environment.
- **WAF, Shield Advanced, GuardDuty and backups to another region or account:** not included.
- **Database user:** the services and migrations use the master user. A separate least-privilege application role needs a PostgreSQL provider or a one-off SQL task.
- **Database TLS:** connections use `sslmode=require` (encrypted, server not verified). `verify-full` needs the RDS CA bundle in the image.
- **Internal traffic:** the console reaches the API over plain HTTP inside the VPC. Use an internal ALB with TLS if internal traffic must be encrypted too.
- **Base images:** the Dockerfiles use tags (`golang:1.25`, `distroless/static-debian12:nonroot`, `node:22-bookworm-slim`). Pin them by digest for reproducible builds, and keep `golang:1.25` on its latest patch release for the standard-library fixes in CLEANUP_PLAN.md (V1).
- **CI:** none in the repo. The scripts are written so a pipeline can call `deploy.sh --yes` with an approval step before it.
- **Validation:** `terraform fmt -check` passes. `terraform validate` and `plan` have not run: they need `terraform init` (provider downloads) and AWS credentials.
