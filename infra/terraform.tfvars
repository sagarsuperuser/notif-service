env               = "prod-test"
admin_cidr        = "116.0.0.0/8"
key_name          = "apps"
pause_environment = false

# One server + two workers. The five-role pool layout the benchmark campaigns
# used (and the 22-vCPU quota arithmetic that sized it) is recorded in
# docs/campaign-100k and docs/benchmark-2026-08-14.md; the running system
# does not need it.
worker_count = 2

# Spot by default. The campaigns ran on-demand so a spot interruption could
# not silently change a measurement; that reasoning lives with the campaign
# docs. Set worker_on_demand_percentage = 100 to reproduce a measurement run.
worker_on_demand_percentage = 0

# Non-burstable types — t-family CPU credits make sustained behaviour depend
# on prior idle time (see variables.tf).
# k3s_server_instance_type = "m7i.large"
# worker_instance_types    = ["c7i.large", "c6i.large"]
# db_instance_class        = "db.m7g.large"
