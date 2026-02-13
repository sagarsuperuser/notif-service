env                                = "prod-test"
bastion_ssh_cidr                   = "116.75.0.0/16"
key_name                           = "apps"
bastion_instance_type              = "t3.small"
k3s_server_instance_type           = "t3.medium"
k3s_agent_monitoring_instance_type = "t3.medium"
k3s_agent_ondemand_instance_type   = "t3.small"
k3s_agent_ondemand_count           = 7
k3s_worker_spot_count              = 6
k3s_mock_provider_spot_count       = 7
k3s_general_spot_count             = 3
k3s_monitoring_agent_count         = 1
k3s_mock_provider_agent_count      = 2
k3s_worker_agent_count             = 2

# RDS sizing (8 GiB RAM)
db_instance_class = "db.t4g.large"
