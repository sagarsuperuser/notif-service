-- 1) Tenants
INSERT INTO tenants (id, name)
VALUES ('foodapp', 'Food App (Local)')
ON CONFLICT (id) DO NOTHING;

-- 2) Consents (so you can test opted_in vs opted_out)
INSERT INTO consents (tenant_id, phone, channel, status)
SELECT 'foodapp', '+199900' || lpad(gs::text, 5, '0'), 'sms', 'opted_in'
FROM generate_series(0, 99999) AS gs
ON CONFLICT (tenant_id, phone, channel)
DO UPDATE SET status = EXCLUDED.status, updated_at = now();

-- Explicit opted-out example
INSERT INTO consents (tenant_id, phone, channel, status)
VALUES ('foodapp', '+918888888888', 'sms', 'opted_out')
ON CONFLICT (tenant_id, phone, channel)
DO UPDATE SET status = EXCLUDED.status, updated_at = now();

-- 3) Suppression list (hard block test)
INSERT INTO suppression_list (tenant_id, phone, reason)
VALUES
  ('foodapp', '+917777777777', 'manual_block')
ON CONFLICT (tenant_id, phone)
DO UPDATE SET reason = EXCLUDED.reason, created_at = now();
