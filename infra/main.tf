provider "aws" {
  region = var.region
}

data "aws_availability_zones" "azs" {
  state = "available"
}

locals {
  name = "${var.project}-${var.env}"
  azs  = slice(data.aws_availability_zones.azs.names, 0, var.az_count)

  public_subnet_cidrs  = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 8, i)]
  private_subnet_cidrs = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 8, i + 10)]
}

# -------------------------
# VPC + Subnets
# -------------------------
resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = local.name }
}

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "${local.name}-igw" }
}

resource "aws_subnet" "public" {
  for_each = { for i, az in local.azs : az => i }

  vpc_id                  = aws_vpc.this.id
  availability_zone       = each.key
  cidr_block              = local.public_subnet_cidrs[each.value]
  map_public_ip_on_launch = true

  tags = {
    Name = "${local.name}-public-${each.key}"
  }
}

resource "aws_subnet" "private" {
  for_each = { for i, az in local.azs : az => i }

  vpc_id                  = aws_vpc.this.id
  availability_zone       = each.key
  cidr_block              = local.private_subnet_cidrs[each.value]
  map_public_ip_on_launch = false

  tags = {
    Name = "${local.name}-private-${each.key}"
  }
}

# Public route table
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "${local.name}-public-rt" }
}

resource "aws_route" "public_igw" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.igw.id
}

resource "aws_route_table_association" "public_assoc" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

# NAT (one NAT for MVP)
resource "aws_eip" "nat" {
  domain = "vpc"
  tags   = { Name = "${local.name}-nat-eip" }
}

resource "aws_nat_gateway" "nat" {
  allocation_id = aws_eip.nat.id
  subnet_id     = values(aws_subnet.public)[0].id
  tags          = { Name = "${local.name}-nat" }
  depends_on    = [aws_internet_gateway.igw]
}

# Private route table
resource "aws_route_table" "private" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "${local.name}-private-rt" }
}

resource "aws_route" "private_nat" {
  route_table_id         = aws_route_table.private.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.nat.id
}

resource "aws_route_table_association" "private_assoc" {
  for_each       = aws_subnet.private
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private.id
}

# -------------------------
# Security Groups
# -------------------------
resource "aws_security_group" "bastion" {
  name        = "${local.name}-bastion-sg"
  description = "Bastion SG"
  vpc_id      = aws_vpc.this.id

  # OPTIONAL SSH from your IP (if you use SSH).
  dynamic "ingress" {
    for_each = var.bastion_ssh_cidr == null ? [] : [1]
    content {
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = [var.bastion_ssh_cidr]
    }
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "${local.name}-bastion-sg" }
}

resource "aws_security_group" "nodes" {
  name        = "${local.name}-nodes-sg"
  description = "k3s nodes SG"
  vpc_id      = aws_vpc.this.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "${local.name}-nodes-sg" }
}

# SSH from bastion (optional)
resource "aws_security_group_rule" "nodes_ssh_from_bastion" {
  count                    = var.key_name == null ? 0 : 1
  type                     = "ingress"
  security_group_id        = aws_security_group.nodes.id
  from_port                = 22
  to_port                  = 22
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.bastion.id
}

# node-to-node (self-referencing rules)
resource "aws_security_group_rule" "nodes_etcd" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = 2379
  to_port           = 2380
  protocol          = "tcp"
  self              = true
}

resource "aws_security_group_rule" "nodes_kubelet" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = 10250
  to_port           = 10250
  protocol          = "tcp"
  self              = true
}

resource "aws_security_group_rule" "nodes_flannel" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = 8472
  to_port           = 8472
  protocol          = "udp"
  self              = true
}

resource "aws_security_group_rule" "nodes_nodeports" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = 30000
  to_port           = 32767
  protocol          = "tcp"
  self              = true
}

# Internal API NLB SG (so only bastion can hit 6443)
resource "aws_security_group" "api_nlb" {
  name        = "${local.name}-api-nlb-sg"
  description = "Internal API NLB SG"
  vpc_id      = aws_vpc.this.id

  egress {
    from_port       = 6443
    to_port         = 6443
    protocol        = "tcp"
    security_groups = [aws_security_group.nodes.id]
  }

  tags = { Name = "${local.name}-api-nlb-sg" }
}

resource "aws_security_group_rule" "api_nlb_6443_from_bastion" {
  type                     = "ingress"
  security_group_id        = aws_security_group.api_nlb.id
  from_port                = 6443
  to_port                  = 6443
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.bastion.id
}

resource "aws_security_group_rule" "api_nlb_6443_from_nodes" {
  type                     = "ingress"
  security_group_id        = aws_security_group.api_nlb.id
  from_port                = 6443
  to_port                  = 6443
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.nodes.id
}
# Nodes allow 6443 only from api-nlb SG + node self (k3s server comms)
resource "aws_security_group_rule" "nodes_6443_from_api_nlb" {
  type                     = "ingress"
  security_group_id        = aws_security_group.nodes.id
  from_port                = 6443
  to_port                  = 6443
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.api_nlb.id
}

resource "aws_security_group_rule" "nodes_6443_from_nodes" {
  type                     = "ingress"
  security_group_id        = aws_security_group.nodes.id
  from_port                = 6443
  to_port                  = 6443
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.nodes.id
}

# Internet-facing Ingress NLB SG
resource "aws_security_group" "ingress_nlb" {
  name        = "${local.name}-ingress-nlb-sg"
  description = "Ingress NLB SG"
  vpc_id      = aws_vpc.this.id

  egress {
    from_port       = var.ingress_http_nodeport
    to_port         = var.ingress_https_nodeport
    protocol        = "tcp"
    security_groups = [aws_security_group.nodes.id]
  }

  tags = { Name = "${local.name}-ingress-nlb-sg" }
}

resource "aws_security_group_rule" "ingress_nlb_80_from_world" {
  type              = "ingress"
  security_group_id = aws_security_group.ingress_nlb.id
  from_port         = 80
  to_port           = 80
  protocol          = "tcp"
  cidr_blocks       = ["0.0.0.0/0"]
}

resource "aws_security_group_rule" "ingress_nlb_443_from_world" {
  type              = "ingress"
  security_group_id = aws_security_group.ingress_nlb.id
  from_port         = 443
  to_port           = 443
  protocol          = "tcp"
  cidr_blocks       = ["0.0.0.0/0"]
}

resource "aws_security_group_rule" "nodes_nodeports_from_ingress_nlb" {
  type                     = "ingress"
  security_group_id        = aws_security_group.nodes.id
  from_port                = 30080
  to_port                  = 30443
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.ingress_nlb.id
}

# RDS SG
resource "aws_security_group" "rds" {
  name        = "${local.name}-rds-sg"
  description = "RDS SG"
  vpc_id      = aws_vpc.this.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "${local.name}-rds-sg" }
}

resource "aws_security_group_rule" "rds_5432_from_nodes" {
  type                     = "ingress"
  security_group_id        = aws_security_group.rds.id
  from_port                = 5432
  to_port                  = 5432
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.nodes.id
}

# -------------------------
# AMI (Ubuntu 24.04)
# -------------------------
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
}

# -------------------------
# k3s token
# -------------------------
resource "random_password" "k3s_token" {
  length  = 32
  special = false
}

# -------------------------
# NLBs
# -------------------------

# API NLB (internal)
resource "aws_lb" "api" {
  name               = "${local.name}-api"
  internal           = true
  load_balancer_type = "network"
  subnets            = [for s in aws_subnet.private : s.id]

  # If your account/region supports NLB SGs, keep this.
  # If it errors, remove `security_groups` and rely on nodes SG allowing VPC CIDR instead.
  security_groups = [aws_security_group.api_nlb.id]

  tags = { Name = "${local.name}-api-nlb" }
}

resource "aws_lb_target_group" "api_6443" {
  name        = "${local.name}-api-6443"
  port        = 6443
  protocol    = "TCP"
  vpc_id      = aws_vpc.this.id
  target_type = "instance"

  health_check {
    protocol = "TCP"
    port     = "6443"
  }
}

resource "aws_lb_listener" "api_6443" {
  load_balancer_arn = aws_lb.api.arn
  port              = 6443
  protocol          = "TCP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api_6443.arn
  }
}

# Ingress NLB (internet-facing)
resource "aws_lb" "ingress" {
  name               = "${local.name}-ingress"
  internal           = false
  load_balancer_type = "network"
  subnets            = [for s in aws_subnet.public : s.id]
  security_groups    = [aws_security_group.ingress_nlb.id]
  tags               = { Name = "${local.name}-ingress-nlb" }
}

resource "aws_lb_target_group" "ingress_http" {
  name        = "${local.name}-ing-http"
  port        = var.ingress_http_nodeport
  protocol    = "TCP"
  vpc_id      = aws_vpc.this.id
  target_type = "instance"

  health_check {
    protocol = "TCP"
    port     = tostring(var.ingress_http_nodeport)
  }
}

resource "aws_lb_target_group" "ingress_https" {
  name        = "${local.name}-ing-https"
  port        = var.ingress_https_nodeport
  protocol    = "TCP"
  vpc_id      = aws_vpc.this.id
  target_type = "instance"

  health_check {
    protocol = "TCP"
    port     = tostring(var.ingress_https_nodeport)
  }
}

resource "aws_lb_listener" "ingress_80" {
  load_balancer_arn = aws_lb.ingress.arn
  port              = 80
  protocol          = "TCP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.ingress_http.arn
  }
}

resource "aws_lb_listener" "ingress_443" {
  load_balancer_arn = aws_lb.ingress.arn
  port              = 443
  protocol          = "TCP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.ingress_https.arn
  }
}

# -------------------------
# EC2 instances
# -------------------------
# Bastion (public)
resource "aws_instance" "bastion" {
  ami                         = data.aws_ami.ubuntu.id
  instance_type               = var.instance_type
  subnet_id                   = values(aws_subnet.public)[0].id
  vpc_security_group_ids      = [aws_security_group.bastion.id]
  associate_public_ip_address = true
  key_name                    = var.key_name

  root_block_device {
    volume_size = 20
    volume_type = "gp3"
  }

  tags = {
    Name = "${local.name}-bastion"
    Role = "bastion"
  }
}

# k3s server user_data templates
locals {
  k3s_common = <<-EOT
    #!/bin/bash
    set -euo pipefail
    curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=${var.k3s_version} sh -s - \
  EOT

  server1_user_data = <<-EOT
    ${local.k3s_common} server \
      --cluster-init \
      --token ${random_password.k3s_token.result} \
      --tls-san ${aws_lb.api.dns_name} \
      --write-kubeconfig-mode 644 \
      --disable traefik
  EOT

  server_join_user_data = <<-EOT
    ${local.k3s_common} server \
      --server https://${aws_lb.api.dns_name}:6443 \
      --token ${random_password.k3s_token.result} \
      --tls-san ${aws_lb.api.dns_name} \
      --write-kubeconfig-mode 644 \
      --disable traefik
  EOT

  agent_user_data = <<-EOT
    ${local.k3s_common} agent \
      --server https://${aws_lb.api.dns_name}:6443 \
      --token ${random_password.k3s_token.result}
  EOT
}

# 3 servers (one per AZ)
resource "aws_instance" "k3s_server" {
  count = 3

  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  subnet_id              = values(aws_subnet.private)[count.index].id
  vpc_security_group_ids = [aws_security_group.nodes.id]
  key_name               = var.key_name

  root_block_device {
    volume_size = 50
    volume_type = "gp3"
  }

  user_data = count.index == 0 ? local.server1_user_data : local.server_join_user_data

  tags = {
    Name = "${local.name}-k3s-server-${count.index + 1}"
    Role = "k3s-server"
  }

  depends_on = [aws_lb_listener.api_6443]
}

# Attach servers to API TG
resource "aws_lb_target_group_attachment" "api_servers" {
  count            = 3
  target_group_arn = aws_lb_target_group.api_6443.arn
  target_id        = aws_instance.k3s_server[count.index].id
  port             = 6443
}

# 3 workers (spread across AZs)
resource "aws_instance" "k3s_agent" {
  count = 3

  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  subnet_id              = values(aws_subnet.private)[count.index].id
  vpc_security_group_ids = [aws_security_group.nodes.id]
  key_name               = var.key_name

  root_block_device {
    volume_size = 50
    volume_type = "gp3"
  }

  user_data = local.agent_user_data

  tags = {
    Name = "${local.name}-k3s-agent-${count.index + 1}"
    Role = "k3s-agent"
  }

  depends_on = [aws_lb_listener.api_6443]
}

# Attach workers to ingress target groups
resource "aws_lb_target_group_attachment" "ingress_workers_http" {
  count            = 3
  target_group_arn = aws_lb_target_group.ingress_http.arn
  target_id        = aws_instance.k3s_agent[count.index].id
  port             = var.ingress_http_nodeport
}

resource "aws_lb_target_group_attachment" "ingress_workers_https" {
  count            = 3
  target_group_arn = aws_lb_target_group.ingress_https.arn
  target_id        = aws_instance.k3s_agent[count.index].id
  port             = var.ingress_https_nodeport
}

# -------------------------
# RDS Postgres
# -------------------------
resource "random_password" "db_password" {
  length           = 24
  special          = true
  override_special = "!#$%&()*+,-.:;<=>?[]^_{|}~"
}

resource "aws_db_subnet_group" "db" {
  name       = "${local.name}-db-subnets"
  subnet_ids = [for s in aws_subnet.private : s.id]
}

resource "aws_db_instance" "postgres" {
  identifier        = "${local.name}-postgres"
  engine            = "postgres"
  engine_version    = "17.6"
  instance_class    = var.db_instance_class
  allocated_storage = 50
  storage_type      = "gp3"

  db_name  = var.db_name
  username = var.db_username
  password = random_password.db_password.result

  publicly_accessible = false
  skip_final_snapshot = true
  deletion_protection = false

  vpc_security_group_ids = [aws_security_group.rds.id]
  db_subnet_group_name   = aws_db_subnet_group.db.name

  tags = { Name = "${local.name}-postgres" }
}

# -------------------------
# SQS FIFO + DLQ
# -------------------------
resource "aws_sqs_queue" "dlq" {
  name                        = "${local.name}-send-dlq.fifo"
  fifo_queue                  = true
  content_based_deduplication = true
}

resource "aws_sqs_queue" "main" {
  name                        = "${local.name}-send.fifo"
  fifo_queue                  = true
  content_based_deduplication = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = 5
  })
}
