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

CREATE INDEX IF NOT EXISTS idx_messages_tenant_campaign_created ON messages (tenant_id, campaign_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_tenant_phone_created ON messages (tenant_id, to_phone, created_at);
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

CREATE INDEX IF NOT EXISTS idx_delivery_events_provider_msg ON delivery_events (provider, provider_msg_id);
