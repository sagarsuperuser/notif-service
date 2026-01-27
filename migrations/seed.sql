-- 1) Tenants
INSERT INTO tenants (id, name)
VALUES ('foodapp', 'Food App (Local)')
ON CONFLICT (id) DO NOTHING;

-- 2) Consents (so you can test opted_in vs opted_out)
-- Use these numbers consistently in curl tests.
INSERT INTO consents (tenant_id, phone, channel, status)
VALUES
  ('foodapp', '+919003021770', 'sms', 'opted_in'),
  ('foodapp', '+918888888888', 'sms', 'opted_out')
ON CONFLICT (tenant_id, phone, channel)
DO UPDATE SET status = EXCLUDED.status, updated_at = now();

-- 3) Suppression list (hard block test)
INSERT INTO suppression_list (tenant_id, phone, reason)
VALUES
  ('foodapp', '+917777777777', 'manual_block')
ON CONFLICT (tenant_id, phone)
DO UPDATE SET reason = EXCLUDED.reason, created_at = now();
