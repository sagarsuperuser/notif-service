env               = "prod-test"
admin_cidr        = "116.0.0.0/8"
key_name          = "apps"
pause_environment = false

# One server + two workers. The five-role pool layout the benchmark campaigns
# used (and the 22-vCPU quota arithmetic that sized it) is recorded in
# docs/campaign-100k and docs/benchmark-2026-08-14.md; the running system
# does not need it.
worker_count = 2

# On-demand plain-ASG launches only, in THIS account: the spot vCPU quota is
# 1 (L-34B43A08) and the EC2 Fleet-request quota rejects mixed-instances ASGs
# even for on-demand — both found by the verification apply. workers_use_spot
# stays false until a quota increase.
workers_use_spot = false

# Non-burstable types — t-family CPU credits make sustained behaviour depend
# on prior idle time (see variables.tf).
# k3s_server_instance_type = "m7i.large"
# worker_instance_types    = ["c7i.large", "c6i.large"]
# db_instance_class        = "db.m7g.large"
