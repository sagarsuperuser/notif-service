# Architecture Diagrams

This folder contains production-oriented architecture views for `notif-service`.

## Diagrams

1. `01-system-context.md`
   - C4-style system context (external actors and major systems).

2. `02-container-runtime.md`
   - Kubernetes and AWS runtime/container view.

3. `03-message-lifecycle-sequence.md`
   - End-to-end message and webhook processing sequence.

4. `04-scaling-failure-domains.md`
   - Node pools, autoscaling boundaries, and failure behavior.

## How to use

- View directly in Markdown tools that support Mermaid.
- Export to PNG/SVG for reports and runbooks.
- Keep these diagrams updated when changing:
  - queue topology
  - webhook mode
  - worker scaling (KEDA on queue depth)
  - node topology (one server + one worker ASG)
  - database topology (direct RDS; pgx pooling in the services)
