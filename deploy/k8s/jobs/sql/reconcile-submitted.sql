-- Reconcile messages stuck in 'submitted' based on the latest terminal delivery event.
--
-- This is intentionally idempotent: re-running produces the same final state.
-- "Latest terminal event wins" (by received_at).

WITH last_terminal AS (
  SELECT DISTINCT ON (provider, provider_msg_id)
    provider,
    provider_msg_id,
    vendor_status,
    error_code,
    received_at
  FROM delivery_events
  WHERE vendor_status IN ('delivered', 'failed', 'undelivered')
  ORDER BY provider, provider_msg_id, received_at DESC
)
UPDATE messages m
SET
  state = CASE last_terminal.vendor_status
            WHEN 'delivered' THEN 'delivered'
            ELSE 'failed'
          END,
  last_error = NULLIF(last_terminal.error_code, ''),
  updated_at = now()
FROM last_terminal
WHERE m.state = 'submitted'
  AND m.provider = last_terminal.provider
  AND m.provider_msg_id = last_terminal.provider_msg_id;

