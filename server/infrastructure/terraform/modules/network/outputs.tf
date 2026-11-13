output "database_subnet_ids" {
  description = "Subnets for the database subnet group"
  value       = aws_subnet.database[*].id
}

output "nat_public_ips" {
  description = "Public addresses the platform's outbound traffic comes from (for allow-lists at webhook receivers)"
  value       = aws_eip.nat[*].public_ip
}

output "private_subnet_ids" {
  description = "Subnets for ECS tasks"
  value       = aws_subnet.private[*].id
}

output "public_subnet_ids" {
  description = "Subnets for the load balancer"
  value       = aws_subnet.public[*].id
}

output "vpc_cidr" {
  description = "CIDR block of the VPC"
  value       = aws_vpc.main.cidr_block
}

output "vpc_id" {
  description = "ID of the VPC"
  value       = aws_vpc.main.id
}
