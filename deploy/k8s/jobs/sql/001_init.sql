CREATE TABLE IF NOT EXISTS tenants (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS consents (
  tenant_id  TEXT NOT NULL,
  phone      TEXT NOT NULL,
  channel    TEXT NOT NULL, -- "sms"
  status     TEXT NOT NULL, -- "opted_in" | "opted_out"
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, phone, channel)
);

CREATE TABLE IF NOT EXISTS suppression_list (
  tenant_id  TEXT NOT NULL,
  phone      TEXT NOT NULL,
  reason     TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, phone)
);

CREATE TABLE IF NOT EXISTS send_caps_daily (
  tenant_id  TEXT NOT NULL,
  phone      TEXT NOT NULL,
  day        DATE NOT NULL,
  count      INT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, phone, day)
);

CREATE TABLE IF NOT EXISTS messages (
  id               TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  idempotency_key  TEXT NOT NULL,
  to_phone         TEXT NOT NULL,
  template_id      TEXT NOT NULL,
  vars_json        JSONB NOT NULL,
  campaign_id      TEXT NULL,
  state            TEXT NOT NULL, -- queued|processing|suppressed|submitted|delivered|failed
  provider         TEXT NULL,      -- "twilio"
  provider_msg_id  TEXT NULL,      -- Twilio MessageSid
  last_error       TEXT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, idempotency_key)
);

-- messages is the hottest table in the system: every accepted send inserts a
-- row and the worker updates it twice more. Every secondary index is
-- maintained on each of those writes, so an index that no query uses is a pure
-- tax on throughput.
--
-- The campaign and phone indexes were dropped because nothing reads them: no
-- query in the service or the jobs filters messages by campaign_id or
-- to_phone. If campaign reporting arrives, the index comes back together with
-- the query that needs it, and can be shaped to that query rather than guessed
-- at in advance.
DROP INDEX IF EXISTS idx_messages_tenant_campaign_created;
DROP INDEX IF EXISTS idx_messages_tenant_phone_created;

-- Used by the webhook path (UPDATE ... WHERE provider = $1 AND provider_msg_id
-- = $2) and by reconcile's join back to messages.
CREATE INDEX IF NOT EXISTS idx_messages_provider_msg_id ON messages (provider, provider_msg_id);

CREATE TABLE IF NOT EXISTS provider_attempts (
  id              BIGSERIAL PRIMARY KEY,
  message_id      TEXT NOT NULL REFERENCES messages(id),
  provider        TEXT NOT NULL,
  provider_msg_id TEXT NULL,
  http_status     INT NULL,
  error_code      TEXT NULL,
  error_msg       TEXT NULL,
  request_json    JSONB NULL,
  response_json   JSONB NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS delivery_events (
  id            BIGSERIAL PRIMARY KEY,
  provider      TEXT NOT NULL,
  provider_msg_id TEXT NOT NULL,
  message_id    TEXT NULL,
  vendor_status TEXT NOT NULL,
  error_code    TEXT NULL,
  payload_json  JSONB NOT NULL,
  occurred_at   TIMESTAMPTZ NULL,
  received_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- reconcile-submitted picks the latest TERMINAL event per provider message:
--
--   SELECT DISTINCT ON (provider, provider_msg_id) ...
--     WHERE vendor_status IN ('delivered','failed','undelivered')
--     ORDER BY provider, provider_msg_id, received_at DESC
--
-- This index matches that shape exactly — the leading columns are the DISTINCT
-- ON key, received_at DESC gives the per-key ordering without a sort, and the
-- WHERE clause makes it a partial index so non-terminal events (the majority
-- during a send burst, since every message reports 'queued' and 'sent' before
-- it reports anything final) cost nothing to maintain and take up no space.
--
-- It replaces a full index on (provider, provider_msg_id), which covered every
-- event row and still left reconcile sorting.
--
-- Deliberately NOT added: a partial index on messages WHERE state='submitted',
-- which would let reconcile find its candidates without a scan. Every message
-- passes through 'submitted', so such an index would be inserted into and
-- deleted from on the hot path — paying on every send to speed up a cron job
-- that runs on its own schedule and is allowed to take its time.
DROP INDEX IF EXISTS idx_delivery_events_provider_msg;
CREATE INDEX IF NOT EXISTS idx_delivery_events_terminal
  ON delivery_events (provider, provider_msg_id, received_at DESC)
  WHERE vendor_status IN ('delivered', 'failed', 'undelivered');
