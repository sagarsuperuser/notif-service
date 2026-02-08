-- 1) Tenants
INSERT INTO tenants (id, name)
VALUES ('foodapp', 'Food App (Local)')
ON CONFLICT (id) DO NOTHING;

-- 2) Consents (so you can test opted_in vs opted_out)
-- Read opted-in phone pool from mounted file /sql/phones.txt
DROP TABLE IF EXISTS seed_phones;
CREATE TEMP TABLE seed_phones (
  phone TEXT PRIMARY KEY
);
\copy seed_phones(phone) FROM '/sql/phones.txt' WITH (FORMAT text);

INSERT INTO consents (tenant_id, phone, channel, status)
SELECT 'foodapp', phone, 'sms', 'opted_in'
FROM seed_phones
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
