output "vpc_id" { value = aws_vpc.this.id }

output "api_nlb_dns" {
  value = aws_lb.api.dns_name
}

output "ingress_nlb_dns" {
  value = aws_lb.ingress.dns_name
}

output "bastion_public_ip" {
  value = try(aws_instance.bastion[0].public_ip, null)
}

output "rds_endpoint" {
  value = aws_db_instance.postgres.address
}

output "rds_proxy_endpoint" {
  value = aws_db_proxy.postgres.endpoint
}

output "rds_port" { value = aws_db_instance.postgres.port }
output "db_name" { value = aws_db_instance.postgres.db_name }
output "db_user" { value = aws_db_instance.postgres.username }
output "db_password" {
  value     = local.db_master_password
  sensitive = true
}

output "sqs_main_url" { value = aws_sqs_queue.main.url }
output "sqs_dlq_url" { value = aws_sqs_queue.dlq.url }

output "k3s_token" {
  value     = random_password.k3s_token.result
  sensitive = true
}
