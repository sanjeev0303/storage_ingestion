output "arn" {
  description = "ARN of the bucket"
  value       = aws_s3_bucket.main.arn
}

output "name" {
  description = "Name of the bucket"
  value       = aws_s3_bucket.main.id
}

output "regional_domain_name" {
  description = "Regional domain name of the bucket"
  value       = aws_s3_bucket.main.bucket_regional_domain_name
}
