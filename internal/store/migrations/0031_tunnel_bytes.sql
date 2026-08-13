-- Tunnelled egress metering (#244): the broker relays CONNECT tunnel bytes
-- without inspection, so a tunnel was metered once per CONNECT with no
-- per-request accounting afterwards. The relay sees io.Copy byte counts, so
-- the bytes are persisted here under provider 'tunnel' (rows never collide
-- with provider token rows, and the token-budget day-sum GROUP BY provider
-- ignores them). Rows written before this migration read back with a zeroed
-- counter.
ALTER TABLE usage ADD COLUMN tunnel_bytes INTEGER NOT NULL DEFAULT 0;
