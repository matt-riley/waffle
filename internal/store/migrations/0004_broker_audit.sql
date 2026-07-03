-- Broker audit (phase 4): every credential grant and proxied request is a
-- row. Raw secrets never appear here — only token prefixes and metadata.
CREATE TABLE broker_audit (
    id           INTEGER PRIMARY KEY,
    at           TEXT NOT NULL,
    token_prefix TEXT NOT NULL, -- first 11 chars of the wk_ token
    session      TEXT NOT NULL,
    action       TEXT NOT NULL, -- 'mint' | 'proxy' | 'denied'
    detail       TEXT NOT NULL
) STRICT;
