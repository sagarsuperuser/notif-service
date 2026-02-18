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

# Keep 3 AZs for 3 control-plane nodes
variable "az_count" {
  type    = number
  default = 3
}

variable "k3s_server_count" {
  type        = number
  default     = 3
  description = "Number of k3s server (control-plane/etcd) nodes."
}

variable "pause_environment" {
  type        = bool
  default     = false
  description = "If true, pauses non-core compute. Keeps k3s servers and monitoring on-demand agents running; scales bastion and all other agent pools to zero."
}

variable "instance_types" {
  description = <<-EOT
    Instance type presets used by this stack.

    - bastion: SSH jumpbox
    - k3s_server: control-plane/etcd nodes
    - k3s_agent_default: default on-demand agents
    - k3s_agent_monitoring: dedicated monitoring agents (workload=monitoring)
  EOT

  type = object({
    bastion              = string
    k3s_server           = string
    k3s_agent_default    = string
    k3s_agent_monitoring = string
  })

  default = {
    bastion              = "t3.small"  # 2 vCPU / 2 GiB
    k3s_server           = "t3.medium" # 2 vCPU / 4 GiB
    k3s_agent_default    = "t3.small"  # 2 vCPU / 2 GiB
    k3s_agent_monitoring = "t3.medium" # 2 vCPU / 4 GiB
  }
}

variable "spot_instance_types" {
  description = "Spot instance types to try per pool (mixed instances policy overrides)."

  type = object({
    worker        = list(string)
    mock_provider = list(string)
    general       = list(string)
  })

  default = {
    worker        = ["t3.small", "t3a.small"]
    mock_provider = ["t3.small", "t3a.small"]
    general       = ["t3.small", "t3a.small"]
  }
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

variable "root_volume_size_monitoring_worker_gb" {
  type    = number
  default = 50
}

variable "root_volume_type" {
  type        = string
  default     = "gp3"
  description = "EBS volume type used for EC2 root volumes."
}

variable "bastion_root_volume_size_gb" {
  type        = number
  default     = 20
  description = "Bastion root volume size in GB."
}

# If you still want SSH, set these.
variable "key_name" {
  type    = string
  default = null
}

variable "bastion_ssh_cidr" {
  type    = string
  default = null
}

# k3s
variable "k3s_version" {
  type    = string
  default = "v1.34.3+k3s1"
}

variable "k3s_agents_on_demand" {
  description = <<-EOT
    On-demand k3s agent pool split by workload role.

    These counts are applied by index order at join time via k3s agent flags:
      monitoring -> mock_provider -> worker -> ingress -> general

    - monitoring/mock_provider/worker/ingress nodes are labeled+tainted as workload=<role>.
    - general nodes have no special workload taints/labels (used for API/webhook/etc.).
  EOT

  type = object({
    monitoring    = number
    mock_provider = number
    worker        = number
    ingress       = number
    general       = number
  })

  default = {
    monitoring    = 0
    mock_provider = 0
    worker        = 0
    ingress       = 0
    general       = 0
  }

  validation {
    condition = alltrue([
      var.k3s_agents_on_demand.monitoring >= 0,
      var.k3s_agents_on_demand.mock_provider >= 0,
      var.k3s_agents_on_demand.worker >= 0,
      var.k3s_agents_on_demand.ingress >= 0,
      var.k3s_agents_on_demand.general >= 0,
    ])
    error_message = "k3s_agents_on_demand: all counts must be >= 0"
  }
}

variable "k3s_agents_spot" {
  description = "Spot k3s agent pools. Each pool is a separate ASG so we can apply different labels/taints at join time."

  type = object({
    worker        = number
    mock_provider = number
    general       = number
  })

  default = {
    worker        = 0
    mock_provider = 0
    general       = 0
  }

  validation {
    condition = alltrue([
      var.k3s_agents_spot.worker >= 0,
      var.k3s_agents_spot.mock_provider >= 0,
      var.k3s_agents_spot.general >= 0,
    ])
    error_message = "k3s_agents_spot: all counts must be >= 0"
  }
}

// Spot instance types are now configured via `var.spot_instance_types`.

# Ingress nodeports for NLB target groups
variable "ingress_http_nodeport" {
  type    = number
  default = 30080
}
variable "ingress_https_nodeport" {
  type    = number
  default = 30443
}

variable "ingress_public_http_port" {
  type    = number
  default = 80
}

variable "ingress_public_https_port" {
  type    = number
  default = 443
}

variable "ssh_port" {
  type    = number
  default = 22
}

variable "k3s_api_port" {
  type    = number
  default = 6443
}

variable "k3s_etcd_port_start" {
  type    = number
  default = 2379
}

variable "k3s_etcd_port_end" {
  type    = number
  default = 2380
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

variable "k3s_token_length" {
  type        = number
  default     = 32
  description = "Length for generated k3s shared token."
}

variable "db_password_length" {
  type        = number
  default     = 24
  description = "Length for generated DB master password when db_password is null."
}

# SQS
variable "sqs_send_visibility_timeout_seconds" {
  type        = number
  default     = 60
  description = "Visibility timeout for the send queue (seconds)."
}

variable "sqs_webhook_events_visibility_timeout_seconds" {
  type        = number
  default     = 60
  description = "Visibility timeout for the webhook events queue (seconds)."
}

variable "sqs_send_max_receive_count" {
  type        = number
  default     = 5
  description = "How many times an item can be received from send queue before DLQ."
}

variable "sqs_webhook_events_max_receive_count" {
  type        = number
  default     = 10
  description = "How many times an item can be received from webhook events queue before DLQ."
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

variable "db_instance_class" {
  type    = string
  default = "db.t4g.small" # change if needed
}

variable "db_password" {
  type      = string
  sensitive = true
  default   = null
}

variable "rds_proxy_idle_client_timeout_seconds" {
  type        = number
  default     = 1800
  description = "RDS Proxy idle client timeout in seconds."
}

variable "rds_proxy_max_connections_percent" {
  type        = number
  default     = 90
  description = "Max DB connections RDS Proxy can use, as percent of DB max_connections."
}

variable "rds_proxy_max_idle_connections_percent" {
  type        = number
  default     = 50
  description = "Max idle DB connections RDS Proxy keeps, as percent of max connections."
}

variable "rds_proxy_connection_borrow_timeout_seconds" {
  type        = number
  default     = 120
  description = "RDS Proxy connection borrow timeout in seconds."
}
