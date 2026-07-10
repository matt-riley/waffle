# Agent-Group Gateway Routing Design

## Goal

Complete issue #33 by making gateway conversations use the policy tier named by their persisted channel-group binding.

## Design

`channel_groups.agent_group` is the durable binding: it is created as `main`, survives database reopen, and refers to a session through `session_id`. The gateway will accept an `Agents` map keyed by group name and select an agent only after `GroupFor` loads that record. `Agent` remains a compatibility fallback for callers that configure only the main tier.

The server will build one agent for `main` and one for each configured group, retaining every cleanup function until shutdown. A persisted group whose configured agent is unavailable is rejected; it never falls back to the `main` agent. This fails closed and prevents a restricted session from acquiring a more privileged toolbox.

No channel-to-group assignment UI is included: #34 owns the group-chat routing policy. Existing records remain `main` and retain current behavior.

## Tests

Gateway tests will prove that a persisted non-main group selects its own provider across a database reopen, and that an unknown persisted group is rejected rather than executing with the main agent. Command tests will continue proving the `main` and `cron` tool policies differ.
