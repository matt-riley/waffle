-- The entity model (phase 3), single-owner edition: waffle serves exactly
-- one person. identities are the owner's channel accounts; anything not in
-- that table gets a pairing code and nothing else (docs/plan.md).

CREATE TABLE identities (
    channel     TEXT NOT NULL,
    external_id TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (channel, external_id)
) STRICT;

-- pairings are pending requests. Approving one (host CLI only — that's the
-- proof of ownership) promotes the sender to an identity.
CREATE TABLE pairings (
    code        TEXT PRIMARY KEY,
    channel     TEXT NOT NULL,
    external_id TEXT NOT NULL,
    sender_name TEXT NOT NULL DEFAULT '',
    chat_id     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    UNIQUE (channel, external_id)
) STRICT;

-- channel_groups bind one conversation (a Telegram chat, ...) to an agent
-- group and its active session.
CREATE TABLE channel_groups (
    id          INTEGER PRIMARY KEY,
    channel     TEXT NOT NULL,
    chat_id     TEXT NOT NULL,
    agent_group TEXT NOT NULL DEFAULT 'main',
    session_id  TEXT NOT NULL REFERENCES sessions(id),
    created_at  TEXT NOT NULL,
    UNIQUE (channel, chat_id)
) STRICT;
