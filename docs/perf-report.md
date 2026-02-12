# Notification Load Test Notes

This doc is meant for engineering review after each load test run.

## Run Info
- Date:
- Commit:
- Cluster/environment:
- Topology:
  - Control plane:
  - Workers (on-demand):
  - Workers (spot):
  - Monitoring node isolation:
- Queue model: SQS FIFO (`tenant:b<hash(to)%2000>`)

## What We Ran
| Scenario | Target load | Duration | Why |
|---|---:|---:|---|
| Baseline steady | 56 RPS | 30m | Check stable behavior under normal load |
| Target steady | 100 RPS | 15m | Validate target throughput and SLOs |
| Burst + recovery | 0 -> 150 RPS, hold | ~20m | Measure backlog growth and drain |

Load job used:
- `deploy/k8s/tools/k6/notif-api-56rps-30m.yaml`

Data assumptions:
- 100k deterministic phone pool (`+19990000000` to `+19990099999`)
- DB seed job applied from `deploy/k8s/jobs`

## Core Results (fill after run)
| Scenario | Achieved RPS (avg / peak) | API p95 / p99 | E2E p95 / p99 | API non-2xx | Peak queue depth | Peak oldest age | Drain time | Worker replicas (min->max) |
|---|---|---|---|---|---:|---|---|---|
| Baseline steady |  |  |  |  |  |  |  |  |
| Target steady |  |  |  |  |  |  |  |  |
| Burst + recovery |  |  |  |  |  |  |  |  |

Notes:
- E2E metric here means: **API accepted -> first provider attempt completion**.

## Worker + Provider Breakdown
### Worker
| Scenario | Processed/s | Worker p95 / p99 | Success % | Retry/throttle % | Failure exhausted % | Scale-up lag |
|---|---:|---|---|---|---|---|
| Baseline steady |  |  |  |  |  |  |
| Target steady |  |  |  |  |  |  |
| Burst + recovery |  |  |  |  |  |  |

### Provider
| Scenario | Send/s | Provider p95 / p99 | 2xx % | 429 % | 5xx % | Other errors % |
|---|---:|---|---|---|---|---|
| Baseline steady |  |  |  |  |  |  |
| Target steady |  |  |  |  |  |  |
| Burst + recovery |  |  |  |  |  |  |

## Evidence Captured
- [ ] API RPS graph
- [ ] API latency graph (p95/p99)
- [ ] E2E latency graph (p95/p99)
- [ ] Worker outcomes graph
- [ ] Provider throughput graph
- [ ] Provider latency graph
- [ ] Provider error mix graph
- [ ] Queue depth + oldest age graph
- [ ] Worker replica count graph
- [ ] k6 logs attached

## What We Learned
- Bottleneck seen:
- Why:
- Change tried:
- Improvement:
- Remaining risk:

## Next Actions
1. 
2. 
3. 

---

## Useful Queries
API RPS:
```promql
sum(rate(notif_api_requests_total{endpoint!~"^/(healthz|readyz|metrics)$"}[1m]))
```

E2E p95/p99:
```promql
histogram_quantile(0.95, sum(rate(notif_end_to_end_latency_seconds_bucket[5m])) by (le))
histogram_quantile(0.99, sum(rate(notif_end_to_end_latency_seconds_bucket[5m])) by (le))
```

Worker outcomes:
```promql
sum by (result) (rate(notif_worker_processed_total[1m]))
```

Provider outcomes:
```promql
sum by (result,http_status) (rate(twilio_send_total[1m]))
```

Queue age/depth:
- Use CloudWatch SQS metrics:
  - `ApproximateNumberOfMessagesVisible`
  - `ApproximateAgeOfOldestMessage`
