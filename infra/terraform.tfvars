env               = "prod-test"
bastion_ssh_cidr  = "116.0.0.0/8"
key_name          = "apps"
pause_environment = false

# Instance types (override defaults if you want)
instance_types = {
  bastion              = "t3.small"
  k3s_server           = "t3.medium"
  k3s_agent_default    = "t3.small"
  k3s_agent_monitoring = "t3.medium"
}

spot_instance_types = {
  worker        = ["t3.small", "t3a.small"]
  general       = ["t3.small", "t3a.small"]
  mock_provider = ["t3.small", "t3a.small"]
}

# k3s agents (on-demand)
k3s_agents_on_demand = {
  monitoring    = 1
  mock_provider = 2
  worker        = 2
  ingress       = 1
  general       = 2
}

# k3s agents (spot ASGs)
k3s_agents_spot = {
  worker        = 4
  general       = 7
  mock_provider = 5
}

# RDS sizing (16 GiB RAM / 4 vCPU)
db_instance_class = "db.t4g.xlarge"

# Phase-1 infra knobs (kept explicit for easier env tuning)
root_volume_type                            = "gp3"
bastion_root_volume_size_gb                 = 20
db_storage_type                             = "gp3"
k3s_token_length                            = 32
db_password_length                          = 24
sqs_send_max_receive_count                  = 5
sqs_webhook_events_max_receive_count        = 10
rds_proxy_idle_client_timeout_seconds       = 900
rds_proxy_max_connections_percent           = 70
rds_proxy_max_idle_connections_percent      = 30
rds_proxy_connection_borrow_timeout_seconds = 30
