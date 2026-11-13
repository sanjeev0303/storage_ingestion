module "monitoring" {
  source = "../../modules/monitoring"

  name               = local.name
  kms_key_arn        = module.kms.key_arn
  alarm_emails       = var.alarm_emails
  alb_arn_suffix     = aws_lb.main.arn_suffix
  db_identifier      = module.database.identifier
  cluster_name       = aws_ecs_cluster.main.name
  queues             = module.queues.queue_names
  dead_letter_queues = module.queues.dead_letter_queue_names

  target_groups = {
    api     = aws_lb_target_group.api.arn_suffix
    console = aws_lb_target_group.console.arn_suffix
  }

  service_names = concat(
    ["${local.name}-api", "${local.name}-console"],
    [for name in keys(local.workers) : "${local.name}-${name}"],
  )
}
