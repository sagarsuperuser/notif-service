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

variable "instance_type" {
  type    = string
  default = "t3.small"
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
