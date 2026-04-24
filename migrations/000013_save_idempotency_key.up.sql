BEGIN;
-- 11_create_idempotency_keys.sql
CREATE TABLE IF NOT EXISTS idempotency_keys (
  id TEXT PRIMARY KEY,
  response BYTEA,
  status_code INT,
  expiry TIMESTAMP
);
COMMIT;