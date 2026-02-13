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
),
to_fix AS (
  -- Batch updates so this job stays bounded even on large tables.
  -- Re-running will process the remaining rows.
  SELECT
    m.id,
    last_terminal.vendor_status,
    last_terminal.error_code
  FROM last_terminal
  JOIN messages m
    ON m.provider = last_terminal.provider
   AND m.provider_msg_id = last_terminal.provider_msg_id
  WHERE m.state = 'submitted'
    -- Avoid racing in-flight updates by the worker/webhook.
    AND m.updated_at < now() - interval '10 seconds'
  ORDER BY last_terminal.received_at DESC
  LIMIT 50000
)
UPDATE messages m
SET
  state = CASE to_fix.vendor_status
            WHEN 'delivered' THEN 'delivered'
            ELSE 'failed'
          END,
  last_error = NULLIF(to_fix.error_code, ''),
  updated_at = now()
FROM to_fix
WHERE m.id = to_fix.id;
