# Architecture Diagrams

This folder contains production-oriented architecture views for `notif-service`.
An API accepts a message and puts a send job on a queue. A worker sends it
to the SMS provider (a Twilio mock in this deployment), and a webhook handler
records the delivery status that the provider reports back. All three services write to Postgres.

## Diagrams

Each page answers one question. Page 03 also covers what the worker's
return value tells the queue, so read it before changing how the worker
handles a delivery.

| Page | Question it answers | What it shows |
|---|---|---|
| 1. `01-system-context.md` | Who talks to whom? | C4-style system context (external actors and major systems). |
| 2. `02-container-runtime.md` | What runs where? | Kubernetes and AWS runtime/container view. |
| 3. `03-message-lifecycle-sequence.md` | What happens to one message? | End-to-end message and webhook processing sequence, and what the worker's return value tells the queue. |
| 4. `04-scaling-failure-domains.md` | How does it scale, and what fails together? | Node pools, autoscaling boundaries, and failure behavior. |

## How to use

- View the pages directly in any Markdown tool that renders Mermaid diagrams.
- Export them to PNG or SVG for reports and runbooks.

### Keeping the diagrams current

Update these diagrams when you change any of the following:

- queue topology
- webhook mode
- worker scaling (KEDA on queue depth)
- node topology (one server + one worker ASG)
- database topology (direct RDS; pgx pooling in the services)
