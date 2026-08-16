---
title: Glossary
description: Every term these docs use, in plain language, with the precise meaning underneath.
sidebar:
  order: 8
---

Plain meaning first. Where a term has an exact definition in Waffle's own
vocabulary, that follows.

## Getting set up

**First Deployment** — going from nothing to a working Waffle. What `waffle
setup` does.

**Managed Setup** — the default: Waffle owns her own internal credentials and
plumbing, and only asks you for things she genuinely cannot generate, like a
model provider key.

**Installed** — she exists: binary, state directory, commands. But she has no
validated model to think with, so she cannot answer. Not a broken state, just an
unfinished one.

**Ready** — Installed, plus a working model that has been checked and selected.
This is the state you want.

**Operator Override** — you taking control of exactly one decision that Managed
Setup would otherwise make, without switching everything else to manual.

## Models and providers

**Provider Connection** — one configured way of reaching a model service:
Anthropic, OpenAI, OpenRouter, a local Ollama. You can have several.

**Model Alias** — a short local name for one specific model, reached through one
Provider Connection. You register these; `/model` only offers what you have
registered, which is why you cannot type an arbitrary model name at it.

## How she is allowed to behave

**Agent Group** — the ceiling. Which tools exist at all, which sandbox applies,
what the network policy is.

**Agent Profile** — a named setup *within* that ceiling: a system prompt, a
model, a set of tools. A profile can be more restrictive than its group. It can
never be less. `main` is the interactive one; others exist for scheduled and
unattended work.

**Policy rule** — one line that allows, denies, or asks before a particular
action. Anything no rule permits is refused.

**Deny by default** — the governing principle. Absence of permission is refusal,
not permission.

## Where work happens

**Sandbox** — a container her tools can run in instead of running directly on
your machine. Sees only the files you gave it; no network unless you listed
specific hosts.

**Broker** — the part that stays on your machine and holds the real credentials.
The sandbox asks it to do things rather than being handed the secret. It issues
short-lived tokens that expire after a day.

**Workspace** — a checked-out code repository she has been given to work in,
with its own container.

**Egress** — what a sandbox may reach on the network. `none` by default;
`allowlist` for named hosts; `full` if you really mean it.

## Remembering

**Session** — one conversation. Searchable afterwards; resumable.

**Working set** — goals and constraints held for the current task only.
Deliberately temporary.

**Durable notes** — permanent memory. The only kind she carries into a new
conversation.

**Write gate** — what happens when she wants to write a durable note: write it
(`auto`), write and tell you (`notify`), or wait for your approval (`review`).
Anything derived from untrusted content is queued regardless.

**Candidate** — a durable write or proposed skill waiting for your decision.
`waffle candidates list`.

## Extending her

**Skill** — a written instruction she can reuse, stored as a `SKILL.md` file.
Ones you write are active immediately; ones she proposes are not.

**Subagent** — a fresh, separate context she spawns for a self-contained
subtask. Not something you invoke; you see it in the transcript. It inherits
your restrictions and can never exceed them.

**MCP server** — an external tool provider she can be connected to. Declared
explicitly; never inherits your environment.

## Running continuously

**`waffle serve`** — the background process that owns scheduled jobs, chat
channels, and the browser view. Scheduled work only happens while it runs.

**Waffle Desk** — the browser view of her state, on your machine only. Off
unless you turn it on.

**Pairing** — binding one of your own accounts on a chat channel to her.
Anybody unpaired gets a code and nothing else.

---

**Nerd corner:** [How it fits together](/docs/under-the-hood/architecture/) —
the same vocabulary with the mechanisms attached.
