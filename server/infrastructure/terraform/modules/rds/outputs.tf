output "address" {
  description = "Host name of the database"
  value       = aws_db_instance.main.address
}

output "database_name" {
  description = "Name of the platform database"
  value       = aws_db_instance.main.db_name
}

output "identifier" {
  description = "RDS instance identifier, for alarms"
  value       = aws_db_instance.main.identifier
}

output "master_username" {
  description = "Master user name"
  value       = aws_db_instance.main.username
}

output "port" {
  description = "Port of the database"
  value       = aws_db_instance.main.port
}

output "security_group_id" {
  description = "Security group of the database"
  value       = aws_security_group.db.id
}
