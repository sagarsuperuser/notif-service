output "vpc_id" { value = aws_vpc.this.id }

output "api_nlb_dns" {
  value = aws_lb.api.dns_name
}

output "ingress_nlb_dns" {
  value = aws_lb.ingress.dns_name
}

output "bastion_public_ip" {
  value = aws_instance.bastion.public_ip
}

output "rds_endpoint" {
  value = aws_db_instance.postgres.address
}

output "rds_port" { value = aws_db_instance.postgres.port }
output "db_name" { value = aws_db_instance.postgres.db_name }
output "db_user" { value = aws_db_instance.postgres.username }
output "db_password" {
  value     = random_password.db_password.result
  sensitive = true
}

output "sqs_main_url" { value = aws_sqs_queue.main.url }
output "sqs_dlq_url" { value = aws_sqs_queue.dlq.url }

output "k3s_token" {
  value     = random_password.k3s_token.result
  sensitive = true
}