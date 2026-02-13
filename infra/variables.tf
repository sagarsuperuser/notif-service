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

variable "bastion_instance_type" {
  type    = string
  default = "t3.small"
}

# k3s node sizing
variable "k3s_server_instance_type" {
  type        = string
  description = "EC2 instance type for k3s server (control-plane/etcd) nodes."
  default     = "t3.medium" # 2 vCPU / 4 GiB
}

variable "k3s_agent_ondemand_instance_type" {
  type        = string
  description = "EC2 instance type for on-demand k3s agent (worker) nodes."
  default     = "t3.small" # 2 vCPU / 2 GiB
}

variable "k3s_agent_monitoring_instance_type" {
  type        = string
  description = "EC2 instance type for dedicated monitoring agent nodes (workload=monitoring)."
  default     = "t3.medium" # 2 vCPU / 4 GiB
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

variable "k3s_agent_ondemand_count" {
  type    = number
  default = 2
}

variable "k3s_worker_spot_count" {
  type        = number
  default     = 0
  description = "How many spot k3s worker nodes to run in an Auto Scaling Group."
}

variable "k3s_agent_spot_instance_types" {
  type        = list(string)
  description = "Spot worker instance types to try (helps with capacity shortages)."
  default     = ["t3.small", "t3a.small"]
}

variable "k3s_mock_provider_spot_count" {
  type        = number
  default     = 0
  description = "How many spot k3s agent nodes to dedicate to mock-provider workload (labeled + tainted at join time)."
}

variable "k3s_mock_provider_spot_instance_types" {
  type        = list(string)
  description = "Spot mock-provider instance types to try."
  default     = ["t3.small", "t3a.small"]
}

variable "k3s_general_spot_count" {
  type        = number
  default     = 0
  description = "How many spot k3s agent nodes to run with no special taints/labels (general pool for API/webhook/etc.)."
}

variable "k3s_general_spot_instance_types" {
  type        = list(string)
  description = "Spot general pool instance types to try."
  default     = ["t3.small", "t3a.small"]
}

variable "k3s_worker_agent_count" {
  type        = number
  default     = 0
  description = "How many on-demand agents to dedicate to worker workload (labeled + tainted at join time). These come after monitoring agents."
}

variable "k3s_mock_provider_agent_count" {
  type        = number
  default     = 0
  description = "How many on-demand agents to dedicate to mock-provider workload (labeled + tainted at join time). These come after monitoring agents."
}

variable "k3s_monitoring_agent_count" {
  type        = number
  default     = 0
  description = "How many agent nodes to dedicate to monitoring (labeled + tainted at join time)."
}

# Ingress nodeports for NLB target groups
variable "ingress_http_nodeport" {
  type    = number
  default = 30080
}
variable "ingress_https_nodeport" {
  type    = number
  default = 30443
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
variable "db_instance_class" {
  type    = string
  default = "db.t4g.small" # change if needed
}

variable "db_password" {
  type      = string
  sensitive = true
  default   = null
}
