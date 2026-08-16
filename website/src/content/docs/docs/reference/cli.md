---
title: Command reference
description: Every waffle subcommand, what it does, and where the longer explanation lives.
sidebar:
  order: 1
---

*In plain terms: [Meet Waffle](/docs/meet/what-waffle-is/).*

Every subcommand the binary accepts. A Go test
(`TestCLIReferenceCoversEveryCommand`) fails if a command exists without an
entry here, or an entry exists without a command — so this page cannot silently
fall behind the binary.

State lives in `$WAFFLE_HOME`, default `~/.waffle`. The global flag
`-c` / `--config` selects a config file (default `$WAFFLE_HOME/config.toml`,
also settable with `WAFFLE_CONFIG`).

## setup

First run: creates the secret-store identity, enrolls a Provider Connection,
writes a starter `[agent.profile.main]`, and offers to enable Waffle Desk.
Re-running is safe — completed steps are detected and skipped.

Walkthrough: [Bringing her home](/docs/meet/bringing-her-home/).

## chat

The focused terminal conversation. Flags: `--continue` resumes the last
session, `--profile <name>` selects an agent profile, `--socket` attaches to a
running service instead of opening state directly, and `--plain` selects the
non-TUI renderer.

Detail: [Chat clients](/docs/under-the-hood/chat-clients/).

## serve

Runs the gateway and all background processing: scheduled jobs, chat channels,
broker tokens, and Waffle Desk. It is the sole owner of that work and holds the
serve-owner lock, so a second `serve` refuses to start.

## status

Shows active and recent gateway runs, token totals, and retry state. Not a
liveness probe — that is `/healthz`.

## pair

Approves your own accounts on connected channels. `waffle pair approve <code>
[name]` binds the code a channel issued. Anyone unapproved gets a code and
nothing else.

## ws

Repository workspaces: `open`, `ls`, `idle`, `close`. Each workspace is a clone
in its own container and volume, with deny-by-default egress.

## cron

Scheduled jobs: `add`, `ls`, `run`, `rm`. Jobs fire only while `serve` is
running. `--deliver channel:chat_id` sends the reply somewhere; `--profile`
scopes the job to an agent profile.

Detail: [Skills, profiles, and jobs](/docs/under-the-hood/skills-profiles-and-jobs/).

## session

Past conversations: `session ls` lists them, `session rm <id>` removes one, and
`session profile <chat> <name>` binds a channel conversation to an agent
profile.

## forget

`waffle forget <query>` previews matching conversation turns and memory notes,
then deletes only what you confirm. It cannot reach provider logs, already
delivered messages, or existing backups.

## usage

Persisted token and request usage, with cost where the provider reports it.

## pause

Stops new agent runs. Nothing is lost and nothing is deleted.

## resume

Resumes agent runs after `pause`.

## secret

The encrypted secret store: initialise it, set and remove values, and export
the identity. `config.toml` only ever holds `secret://` references, never
values.

## mcp

Authorises and manages remote MCP servers: `login`, `status`, `logout`.

## provider

Provider Connections and Model Aliases: add, list, test, and remove
connections; browse a catalogue; register the aliases that `/model` offers.

## backup

`waffle backup <absolute-dir>` writes a local state backup. The encryption
identity is **not** included unless you pass `--with-identity` — otherwise
export it separately with `waffle secret export-identity`, or the backup cannot
be decrypted.

## restore

`waffle restore <absolute-dir>` validates and restores a backup.

## doctor

Runs the self-checks: configuration, secret store, provider reachability, and
sandbox round-trip where Docker mode is configured. The first thing to run when
something is wrong.

## eval

Runs the zero-network agent eval harness, exiting non-zero on failure.
`waffle eval --history` shows recorded runs.

## skills

Skill utilities: `audit`, `activate`, `deactivate`, `ls`. Skills you write by
hand are active immediately; proposals from the learning loop are written
inactive and need explicit activation.

## learn

The mine → propose → validate learning loop. Mines recent sessions for
recurring friction, proposes skills with evidence, and writes them **inactive**.
Identical to `waffle skills audit`.

## upgrade

Rebuilds from an approved ref, verifies, and atomically swaps in the new binary.
`--no-verify` skips vet, tests, and lint, and is unsafe.

## rollback

Restores the previous binary after an `upgrade`.

## completion

Generates shell completion scripts: `bash`, `zsh`, or `fish`.

## version

Prints the version.

## help

Prints the command list. `waffle` with no arguments does the same.

## runner

Not listed in `waffle help`, and deliberately so: `runner` is the in-container
entrypoint the sandbox executes, not a command you invoke. It reads and writes
the paired SQLite queues described in [The sandbox
queue](/docs/under-the-hood/sandbox/).
