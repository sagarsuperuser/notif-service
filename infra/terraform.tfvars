env = "prod-test"

# This AWS account carries a hold on creating load balancers — the API returns
# OperationNotPermitted on CreateLoadBalancer, and it is an account flag rather
# than a quota, so only AWS Support can lift it. Until then the control plane is
# a single server reached on its private IP, and the ingress is reached on its
# NodePort.
use_load_balancers = false

# A single control-plane node, which is what having no load balancer forces:
# there is no stable address for a second server to join through. The control
# plane schedules pods and does not carry requests, so this costs availability
# and not throughput.
k3s_server_count  = 1
bastion_ssh_cidr  = "116.0.0.0/8"
key_name          = "apps"
pause_environment = false

# Instance types.
#
# Everything that carries load is non-burstable. t-family instances accrue CPU
# credits while idle and are throttled to a baseline once those are spent, so a
# sustained run decays partway through and the same test returns a different
# number depending on how long the box sat idle first. That is fine for spiky
# production traffic and useless for a measurement.
#
# x86 rather than Graviton because the node AMI is amd64-only; the container
# images are multi-arch, but the instances would not boot. Not worth the risk
# for the ~10% price difference.
#
# The bastion stays burstable on purpose: it is an SSH entry point, never in
# the data path, and paying for sustained performance there buys nothing.
instance_types = {
  bastion              = "t3.small"
  k3s_server           = "m7i.large"
  k3s_agent_default    = "c7i.large"
  k3s_agent_monitoring = "m7i.large"
}

# Unused: the spot fleets are zeroed below. Kept so the variable stays declared.
spot_instance_types = {
  worker        = ["c7i.large"]
  general       = ["c7i.large"]
  mock_provider = ["c7i.large"]
}

# k3s agents (on-demand).
#
# monitoring is sized for three because the load generator runs there. k6 has
# no node constraints of its own, so it would otherwise schedule onto the
# general pool alongside the API pods — the load generator competing for CPU
# with the component being measured, which makes the result a measurement of
# the test harness. The monitoring pool is tainted, so nothing else lands on it.
#
# Sized to the account's 22 vCPU limit (an increase to 64 is requested and
# pending). Three vCPU are already spent on an unrelated instance and the
# bastion, leaving 19:
#
#   1 server      m7i.large   2
#   1 monitoring  m7i.large   2   <- runs the load generator
#   2 general     c7i.large   4   <- API pods
#   2 worker      c7i.large   4   <- worker pods
#   1 mock        c7i.large   2   <- stands in for the SMS provider
#   1 ingress     c7i.large   2
#                            --
#                             16
#
# The measurement this produces is throughput per core on a named instance
# type, which is the number worth quoting anyway — a large fleet would make the
# same code look better while saying less about it.
k3s_agents_on_demand = {
  monitoring    = 1
  mock_provider = 1
  worker        = 2
  ingress       = 1
  general       = 2
}

# No spot. A spot interruption part-way through a run silently removes capacity
# and the throughput number quietly becomes a number about that interruption.
# Reproducibility is worth more here than the discount.
k3s_agents_spot = {
  worker        = 0
  general       = 0
  mock_provider = 0
}

# RDS sizing (16 GiB RAM / 4 vCPU).
#
# m7g, not t4g. t-family instances are burstable: they accrue CPU credits while
# idle and are throttled to a baseline once those credits are spent. That is a
# reasonable default for spiky production traffic and a bad one for a benchmark,
# because the same test returns different numbers depending on how long the
# instance sat idle beforehand — the run decays partway through and the result
# is not reproducible. A sustained-load measurement needs an instance whose
# performance does not depend on its history.
db_instance_class = "db.m7g.xlarge"

# Phase-1 infra knobs (kept explicit for easier env tuning)
root_volume_type                            = "gp3"
bastion_root_volume_size_gb                 = 20
db_storage_type                             = "gp3"
k3s_token_length                            = 32
db_password_length                          = 24
sqs_send_max_receive_count                  = 5
rds_proxy_idle_client_timeout_seconds       = 900
rds_proxy_max_connections_percent           = 70
rds_proxy_max_idle_connections_percent      = 30
rds_proxy_connection_borrow_timeout_seconds = 30
