output "vpc_id" { value = aws_vpc.this.id }

output "k3s_server_public_ip" {
  description = "Stable public endpoint: kubeconfig target, DNS target for ingress, and the API cert's SAN."
  value       = aws_eip.k3s_server.public_ip
}

output "k3s_server_private_ip" {
  description = "The address agents join through."
  value       = aws_instance.k3s_server.private_ip
}

output "ingress_url" {
  description = "The public entry point (ingress-nginx NodePort behind the server EIP)."
  value       = "http://${aws_eip.k3s_server.public_ip}:${var.ingress_http_nodeport}"
}

output "rds_endpoint" {
  value = aws_db_instance.postgres.address
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
