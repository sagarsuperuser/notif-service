variable "project" {
  type    = string
  default = "notif"
}
variable "env" {
  type    = string
  default = "notif-prod-test"
}
variable "region" {
  type    = string
  default = "ap-south-1"
}

variable "vpc_cidr" {
  type    = string
  default = "10.0.0.0/16"
}

# Two AZs: the minimum an RDS subnet group accepts. Everything else is
# single-instance and does not care.
variable "az_count" {
  type    = number
  default = 2
}

variable "pause_environment" {
  type        = bool
  default     = false
  description = "If true, scales the worker ASG to zero. The k3s server (and anything scheduled on it) keeps running."
}

# Admin access: kubectl (6443) and optional SSH are allowed from this CIDR
# only. There is no bastion — SSM (ssm.tf) is the primary access path and the
# SG is the gate for the rest.
variable "admin_cidr" {
  type        = string
  description = "CIDR allowed to reach the k3s API (6443) and SSH on the nodes."
}

variable "key_name" {
  type    = string
  default = null
}

# Non-burstable on purpose: t-family instances accrue CPU credits while idle
# and are throttled to a baseline once spent, so sustained behaviour depends
# on how long the box sat idle first. (The old tfvars carried this reasoning
# for benchmark runs; it holds for any long-running work.)
variable "k3s_server_instance_type" {
  type    = string
  default = "m7i.large" # 2 vCPU / 8 GiB — control plane + scheduled workloads
}

variable "worker_instance_types" {
  type        = list(string)
  default     = ["c7i.large", "c6i.large"]
  description = "Instance types for the worker ASG (mixed instances policy picks per capacity)."
}

variable "worker_count" {
  type        = number
  default     = 2
  description = "Size of the worker ASG."
}

variable "worker_on_demand_percentage" {
  type        = number
  default     = 0
  description = "0 = all spot (the running default). 100 = all on-demand — use for measurement runs, where a spot interruption would silently change the number."
}

# Root disk sizes (gp3).
variable "root_volume_size_server_gb" {
  type    = number
  default = 50
}

variable "root_volume_size_worker_gb" {
  type    = number
  default = 20
}

variable "root_volume_type" {
  type        = string
  default     = "gp3"
  description = "EBS volume type used for EC2 root volumes."
}

# k3s
variable "k3s_version" {
  type    = string
  default = "v1.34.3+k3s1"
}

variable "k3s_token_length" {
  type        = number
  default     = 32
  description = "Length for generated k3s shared token."
}

# Ingress NodePorts — the public entry point (no load balancer; see main.tf).
variable "ingress_http_nodeport" {
  type    = number
  default = 30080
}
variable "ingress_https_nodeport" {
  type    = number
  default = 30443
}

variable "ssh_port" {
  type    = number
  default = 22
}

variable "k3s_api_port" {
  type    = number
  default = 6443
}

variable "k3s_kubelet_port" {
  type    = number
  default = 10250
}

variable "k3s_flannel_vxlan_port" {
  type    = number
  default = 8472
}

variable "k3s_nodeport_range_start" {
  type    = number
  default = 30000
}

variable "k3s_nodeport_range_end" {
  type    = number
  default = 32767
}

variable "postgres_port" {
  type    = number
  default = 5432
}

# SQS
variable "sqs_send_visibility_timeout_seconds" {
  type        = number
  default     = 60
  description = "Visibility timeout for the send queue (seconds)."
}

variable "sqs_send_max_receive_count" {
  type        = number
  default     = 5
  description = "How many times an item can be received from send queue before DLQ."
}

# RDS
variable "db_name" {
  type    = string
  default = "notif"
}
variable "db_username" {
  type    = string
  default = "notif"
}

variable "db_engine_version" {
  type        = string
  default     = "17.6"
  description = "Postgres engine version."
}

variable "db_allocated_storage_gb" {
  type        = number
  default     = 50
  description = "Allocated storage in GB."
}

variable "db_storage_type" {
  type        = string
  default     = "gp3"
  description = "RDS storage type."
}

# Non-burstable for the same reason as the nodes (m7g, not t4g).
variable "db_instance_class" {
  type    = string
  default = "db.m7g.large"
}

variable "db_password" {
  type      = string
  sensitive = true
  default   = null
}

variable "db_password_length" {
  type        = number
  default     = 24
  description = "Length for generated DB master password when db_password is null."
}
