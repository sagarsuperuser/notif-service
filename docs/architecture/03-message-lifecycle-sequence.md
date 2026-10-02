# 03) Message Lifecycle Sequence

```mermaid
%%{init: {
  "theme": "base",
  "themeVariables": {
    "fontFamily": "Inter, Segoe UI, Arial, sans-serif",
    "fontSize": "18px",
    "primaryTextColor": "#0b1220",
    "lineColor": "#1f2937",
    "actorTextColor": "#0b1220",
    "actorBorder": "#0b3b8c",
    "actorBkg": "#e6f0ff",
    "signalColor": "#1f2937",
    "signalTextColor": "#0b1220",
    "labelBoxBkgColor": "#e6f0ff",
    "labelBoxBorderColor": "#0b3b8c",
    "noteBkgColor": "#fff4cc",
    "noteBorderColor": "#8a6d00",
    "noteTextColor": "#1f1f1f"
  }
}}%%
sequenceDiagram
    autonumber
    participant C as Client
    participant API as notif-api
    participant DB as Postgres (RDS, direct — pgx pools)
    participant QSend as SQS send
    participant W as notif-worker
    participant P as Provider
    participant WH as notif-webhook

    rect rgb(214, 230, 255)
    Note over C,API: API accept path
    C->>API: POST /messages
    API->>DB: Insert message (state=queued)
    API->>QSend: Enqueue send job
    API-->>C: 202 Accepted + message_id
    end

    rect rgb(216, 245, 226)
    Note over QSend,P: Send processing path
    QSend-->>W: Receive job
    Note right of W: Per-worker rate limit before provider call
    W->>P: Send SMS
    P-->>W: Provider response (SID/error)
    W->>DB: Update provider details + state
    Note right of W: Bounded retries + backoff<br/>+ circuit breaker
    end

    rect rgb(255, 228, 214)
    Note over P,WH: Webhook path
    P->>WH: Delivery webhook
    Note over P,WH: Retries only on non-2xx
    WH->>DB: Apply terminal state + store event (one statement)
    WH-->>P: 200 OK
    end

    Note over QSend,W: SQS delivery is at-least-once
```

## What the worker tells the queue

SQS deletes a job only when the worker's handler returns nil. The return value
is therefore an instruction to the queue — *finished* or *not finished* — and
it must agree with what the worker wrote to Postgres. A row the database calls
final cannot be re-claimed, so asking SQS to redeliver it achieves nothing.

| Outcome of one delivery | Row afterwards | Handler returns | Queue then |
|---|---|---|---|
| Provider accepted the send | `submitted` | nil | deletes the job |
| Provider rejected permanently (e.g. 400) | `failed` | nil | deletes the job |
| Temporary failure after 3 in-process attempts (429, 5xx, timeout, refused) | released to `queued` | error | redelivers after the visibility timeout; DLQ after 5 receives |
| Provider says our configuration is wrong (401, 403, 404) | released to `queued` | error | same as above — a bad deploy must not fail messages permanently |
| Circuit breaker open | released to `queued` | error | same as above |
| Template missing (likely a bad deploy) | released to `queued` | error | same as above; redrive once fixed |
| Duplicate delivery of a finished message (`submitted`, `delivered`, `failed`, `suppressed`) | unchanged | nil | deletes the job |
| Duplicate delivery while another worker still holds the row | unchanged | error (`ErrHeldElsewhere`) | redelivers; acknowledged once the holder finishes |
| Sent, but the claim was lost before the result was recorded | attempt recorded; row left to its new owner | nil | deletes the job |
| Job names a message that does not exist | — | error | redelivers, then dead-letters, so it is visible |
| Database write failed | unchanged | error | redelivers |

Every write on a claimed row carries the claim's token (the `updated_at` the
claim wrote) and applies only while the row is still `processing` under it, so
a worker whose claim went stale cannot overwrite the new holder's result or a
`delivered` the webhook already wrote.

Rule: **never return an error after writing a terminal state, and never return
nil while the job still needs a send.** See
[the failure-handling runs](../campaign-100k/retry-handling-ab-2026-08-15.md)
for what breaking it cost.

