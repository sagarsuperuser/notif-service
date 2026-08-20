provider "aws" {
  region = var.region
}

data "aws_availability_zones" "azs" {
  state = "available"
}

# -----------------------------------------------------------------------------
# Topology, and why it is this small (2026-08-20 simplification):
#
#   internet ── DNS ──► server EIP :30080/:30443 (ingress-nginx NodePort)
#   admin    ── SG-locked ──► server :6443 (kubectl) · SSM sessions (no SSH needed)
#   nodes    ── IGW ──► SQS / provider APIs        (public subnets; no NAT)
#   nodes    ──► RDS Postgres :5432                (direct; no RDS Proxy)
#
# Everything that was here before and is gone now, with the reason:
#   - Load balancers (internal API NLB + internet-facing ingress NLB, with
#     their TGs/listeners/SGs and the use_load_balancers toggle): this account
#     cannot create load balancers at all (an account-level hold — the API
#     returns OperationNotPermitted; only AWS Support can lift it), so the
#     stack already ran with the toggle off. The toggle and both code paths
#     are gone: agents join the single server on its private IP, kubectl and
#     the public entry point use the server's EIP, ingress-nginx is reached
#     on its NodePorts. One entry point, no conditional resource graph.
#   - NAT gateway + EIP + private subnets/route tables: NAT bills per GB and
#     every worker→SQS byte paid it. Public subnets with SG-locked ingress
#     behave identically for this workload and SQS via the IGW is free.
#     Nothing accepts inbound except the NodePorts, the admin CIDR, and SSM.
#   - Bastion (+ its SG and rules): ssm.tf already gives key-less, audited
#     access via Session Manager; 6443/SSH are SG-locked to admin_cidr for
#     the cases that want them. A jumpbox guards nothing here.
#   - RDS Proxy (+ its SG, IAM role, Secrets Manager secret): the services'
#     pgx pools hold a handful of long-lived connections; there is no
#     connection storm for a proxy to absorb. The campaign docs that ran
#     behind the proxy are historical records and say so.
#   - 3-server etcd control plane + five role-pinned agent pools + three spot
#     ASGs: workload isolation mattered while benchmarking (the campaign docs
#     record that reasoning); the running system is one server (k3s sqlite) +
#     one worker ASG. The k8s manifests no longer pin workload=
#     nodeSelectors/taints.
#
# Kept on purpose: non-burstable instance types (t-family CPU credits make
# sustained behaviour time-dependent — the original tfvars reasoning holds for
# any long-running work, not just benchmarks), ssm.tf (SSM access + SQS via
# the instance role instead of static keys), SQS FIFO + DLQ, RDS, EBS-CSI IAM.
# -----------------------------------------------------------------------------

locals {
  name = "${var.project}-${var.env}"
  azs  = slice(data.aws_availability_zones.azs.names, 0, var.az_count)

  public_subnet_cidrs = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 8, i)]
  db_master_password  = coalesce(var.db_password, random_password.db_password.result)

  # Pause mode: keep the server (control plane + anything scheduled on it),
  # scale the worker pool to zero.
  effective_worker_count = var.pause_environment ? 0 : var.worker_count
}

# -------------------------
# VPC + public subnets
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

  tags = { Name = "${local.name}-public-${each.key}" }
}

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

# -------------------------
# Security groups
# -------------------------
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

# kubectl / k3s API, admin only.
resource "aws_security_group_rule" "nodes_6443_from_admin" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.k3s_api_port
  to_port           = var.k3s_api_port
  protocol          = "tcp"
  cidr_blocks       = [var.admin_cidr]
}

# agents join the server on 6443 inside the VPC
resource "aws_security_group_rule" "nodes_6443_from_nodes" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.k3s_api_port
  to_port           = var.k3s_api_port
  protocol          = "tcp"
  self              = true
}

# SSH, admin only, only when a key is configured (SSM is the primary path).
resource "aws_security_group_rule" "nodes_ssh_from_admin" {
  count             = var.key_name == null ? 0 : 1
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.ssh_port
  to_port           = var.ssh_port
  protocol          = "tcp"
  cidr_blocks       = [var.admin_cidr]
}

# node-to-node: kubelet, flannel vxlan, nodeport range
resource "aws_security_group_rule" "nodes_kubelet" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.k3s_kubelet_port
  to_port           = var.k3s_kubelet_port
  protocol          = "tcp"
  self              = true
}

resource "aws_security_group_rule" "nodes_flannel" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.k3s_flannel_vxlan_port
  to_port           = var.k3s_flannel_vxlan_port
  protocol          = "udp"
  self              = true
}

resource "aws_security_group_rule" "nodes_nodeports" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.k3s_nodeport_range_start
  to_port           = var.k3s_nodeport_range_end
  protocol          = "tcp"
  self              = true
}

# The public entry point: ingress-nginx NodePorts, world-reachable.
# (There is no load balancer in front — see the header. DNS points at the
# server's EIP; TLS is cert-manager's job inside the cluster.)
resource "aws_security_group_rule" "nodes_ingress_http_from_world" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.ingress_http_nodeport
  to_port           = var.ingress_http_nodeport
  protocol          = "tcp"
  cidr_blocks       = ["0.0.0.0/0"]
}

resource "aws_security_group_rule" "nodes_ingress_https_from_world" {
  type              = "ingress"
  security_group_id = aws_security_group.nodes.id
  from_port         = var.ingress_https_nodeport
  to_port           = var.ingress_https_nodeport
  protocol          = "tcp"
  cidr_blocks       = ["0.0.0.0/0"]
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
  from_port                = var.postgres_port
  to_port                  = var.postgres_port
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
  length  = var.k3s_token_length
  special = false
}

# -------------------------
# IAM for k3s nodes (EBS CSI; ssm.tf attaches SSM + SQS to the same role)
# -------------------------
data "aws_iam_policy_document" "k3s_nodes_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "k3s_nodes" {
  name               = "${local.name}-k3s-nodes"
  assume_role_policy = data.aws_iam_policy_document.k3s_nodes_assume_role.json

  tags = { Name = "${local.name}-k3s-nodes" }
}

resource "aws_iam_role_policy" "k3s_nodes_ebs_csi" {
  name = "${local.name}-k3s-nodes-ebs-csi"
  role = aws_iam_role.k3s_nodes.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ec2:AttachVolume",
          "ec2:CreateSnapshot",
          "ec2:CreateTags",
          "ec2:CreateVolume",
          "ec2:DeleteSnapshot",
          "ec2:DeleteTags",
          "ec2:DeleteVolume",
          "ec2:DescribeAvailabilityZones",
          "ec2:DescribeInstances",
          "ec2:DescribeSnapshots",
          "ec2:DescribeTags",
          "ec2:DescribeVolumes",
          "ec2:DescribeVolumesModifications",
          "ec2:DetachVolume",
          "ec2:ModifyVolume",
        ]
        Resource = "*"
      },
    ]
  })
}

resource "aws_iam_instance_profile" "k3s_nodes" {
  name = "${local.name}-k3s-nodes"
  role = aws_iam_role.k3s_nodes.name
}

# -------------------------
# k3s: one server + one worker ASG
# -------------------------

# Stable public endpoint: kubeconfig, DNS target for ingress, and the cert
# SAN; survives instance replacement. Allocated before the instance so
# user_data can reference it.
resource "aws_eip" "k3s_server" {
  domain = "vpc"
  tags   = { Name = "${local.name}-k3s-server-eip" }
}

locals {
  k3s_common = <<-EOT
    #!/bin/bash
    set -euo pipefail
    curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=${var.k3s_version} sh -s - \
  EOT

  # Single server, k3s's default embedded sqlite (no --cluster-init/etcd).
  # Untainted: the server also schedules workloads.
  server_user_data = <<-EOT
    ${local.k3s_common} server \
      --token ${random_password.k3s_token.result} \
      --tls-san ${aws_eip.k3s_server.public_ip} \
      --write-kubeconfig-mode 644 \
      --disable traefik
  EOT

  agent_user_data = <<-EOT
    ${local.k3s_common} agent \
      --server https://${aws_instance.k3s_server.private_ip}:${var.k3s_api_port} \
      --token ${random_password.k3s_token.result}
  EOT
}

resource "aws_instance" "k3s_server" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.k3s_server_instance_type
  subnet_id              = values(aws_subnet.public)[0].id
  vpc_security_group_ids = [aws_security_group.nodes.id]
  key_name               = var.key_name
  iam_instance_profile   = aws_iam_instance_profile.k3s_nodes.name

  root_block_device {
    volume_size = var.root_volume_size_server_gb
    volume_type = var.root_volume_type
  }

  user_data = local.server_user_data

  tags = {
    Name = "${local.name}-k3s-server"
    Role = "k3s-server"
  }
}

resource "aws_eip_association" "k3s_server" {
  instance_id   = aws_instance.k3s_server.id
  allocation_id = aws_eip.k3s_server.id
}

resource "aws_launch_template" "k3s_worker" {
  name_prefix = "${local.name}-k3s-worker-"
  image_id    = data.aws_ami.ubuntu.id

  # Mixed instances policy overrides this; keep a stable default.
  instance_type = var.worker_instance_types[0]
  key_name      = var.key_name

  user_data              = base64encode(local.agent_user_data)
  vpc_security_group_ids = [aws_security_group.nodes.id]

  iam_instance_profile {
    name = aws_iam_instance_profile.k3s_nodes.name
  }

  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size = var.root_volume_size_worker_gb
      volume_type = var.root_volume_type
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name = "${local.name}-k3s-worker"
      Role = "k3s-agent"
    }
  }
}

resource "aws_autoscaling_group" "k3s_worker" {
  name                = "${local.name}-k3s-worker"
  vpc_zone_identifier = [for s in aws_subnet.public : s.id]

  min_size         = local.effective_worker_count
  max_size         = local.effective_worker_count
  desired_capacity = local.effective_worker_count

  mixed_instances_policy {
    instances_distribution {
      # Spot by default. The old tfvars' no-spot rule protected benchmark
      # reproducibility; those campaigns are historical records now, and the
      # running system prefers the discount. Set 100 to force on-demand for
      # a measurement run.
      on_demand_percentage_above_base_capacity = var.worker_on_demand_percentage
      spot_allocation_strategy                 = "capacity-optimized"
    }

    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.k3s_worker.id
        version            = "$Latest"
      }

      dynamic "override" {
        for_each = var.worker_instance_types
        content {
          instance_type = override.value
        }
      }
    }
  }

  tag {
    key                 = "Name"
    value               = "${local.name}-k3s-worker"
    propagate_at_launch = true
  }

  tag {
    key                 = "Role"
    value               = "k3s-agent"
    propagate_at_launch = true
  }
}

# -------------------------
# RDS Postgres (direct; the pgx pools in the services are the pooling story)
# -------------------------
resource "random_password" "db_password" {
  length  = var.db_password_length
  special = false
}

resource "aws_db_subnet_group" "db" {
  name       = "${local.name}-db-subnets"
  subnet_ids = [for s in aws_subnet.public : s.id]
}

resource "aws_db_instance" "postgres" {
  identifier        = "${local.name}-postgres"
  engine            = "postgres"
  engine_version    = var.db_engine_version
  instance_class    = var.db_instance_class
  allocated_storage = var.db_allocated_storage_gb
  storage_type      = var.db_storage_type
  apply_immediately = true

  db_name  = var.db_name
  username = var.db_username
  password = local.db_master_password

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
  visibility_timeout_seconds  = var.sqs_send_visibility_timeout_seconds

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = var.sqs_send_max_receive_count
  })
}
