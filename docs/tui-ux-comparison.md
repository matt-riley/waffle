# Waffle's TUI/UX against its inspirations

Grounded in the current source (`internal/chatui`, `internal/chat`, `cmd/waffle`) and in current documentation for hermes-agent, OpenClaw, and NanoClaw — the three projects `docs/research.md` names as prior art — plus reviewer coverage of all three from 2026. The short version: architecturally, Waffle is already competitive or ahead of its inspirations (tighten-only trust-tiered profiles, an auditable propose-then-activate learning loop instead of a fully autonomous one, agentskills.io-compatible skills). The gap Matt is feeling isn't missing capability, it's that the interaction surface doesn't yet *show* what's there, and it has a couple of concrete rough edges that read as "broken" rather than "unfinished."

## What the three inspirations actually do in-session

**hermes-agent** keeps a persistent status line above the input at all times: model name, a token-used/token-max fraction with a color-coded fill bar (green under 50%, yellow 50–80%, orange 80–95%, red above that), a running cost estimate, elapsed session time, and badges for active background tasks or an auto-approve ("YOLO") warning. It's visible whether you're idle or mid-turn. Tool calls stream as a live feed (`💻 terminal \`ls -la\` (0.3s)`) rather than only appearing after they finish, and an animated "thinking" indicator fills the dead air during model latency. First-run is one command (`hermes setup --portal`) into `hermes chat`. Slash-command autocomplete opens on typing `/`, and every installed skill is automatically registered as its own slash command (`/github-pr-workflow ...`) rather than needing a `/skill <name>` wrapper. Most relevant to what you flagged: pressing Enter while the agent is still working doesn't do nothing — it's a configurable `/busy` mode. Default is interrupt-and-redirect; `queue` holds your message for the next turn; `steer` injects it into the current run after the next tool call without restarting anything. The first time you ever hit this, Hermes prints a one-line explanation of what just happened and never repeats it.

**OpenClaw** puts almost all of this outside the terminal: a web Control UI on `:18789` for route/session/channel status, onboarding is a guided wizard (`openclaw onboard`) that provisions a workspace with starter prompt files (`AGENTS.md`, `SOUL.md`, `TOOLS.md`, `USER.md`) automatically, and day-to-day status comes from `openclaw status`, `openclaw status --all` (a full, pasteable diagnostic dump), and `openclaw status --deep` (live-probes every channel). Its terminal surface is comparatively thin by design — the dashboard is the product.

**NanoClaw** is the closest architectural sibling to Waffle (container-per-agent, single-writer SQLite IPC, host-side credential vault — the design `docs/research.md` already draws heavily from) and its UX is the cautionary tale, not the model. Independent reviews are consistent: setup takes noticeably longer than advertised, the constant approval prompts during install are described as "tiring," there's no dashboard, and day-two operations are `docker ps` and `tail -f` on raw log files. Reviewers explicitly say it's fine for someone who lives in a terminal and reads `CLAUDE.md`, and not beginner-friendly for anyone else. That's the profile of a project that nailed the architecture and left the interaction layer as an afterthought — which is exactly the risk for Waffle if the TUI stays behind the backend.

The wider 2026 terminal-agent commentary (Claude Code, Cline, opencode-style tools) converges on the same handful of principles: don't make the person hunt for whether the agent is stuck or working, show file edits and commands before/as they happen rather than after, and don't force a rigid interaction model onto what is inherently a "steer this thing while it runs" workflow.

## Where Waffle's current TUI falls short of that bar

Reading `internal/chatui/render.go` and `update.go` directly (not the docs, which describe the intent correctly) turned up one real bug and several honest gaps:

The concrete bug: `submit()` in `update.go` contains `if m.turnActive && !ok { return m, nil }` — if you type a plain message and press Enter while the agent is still working, nothing happens. No error, no queued message, no visual acknowledgment, nothing in the transcript. It's silently swallowed. Compared to hermes-agent's three named busy-input modes (or even just an error toast saying "a turn is active"), this is the single most likely thing to make the chat feel "not quite there" in actual use — you type something reasonable, hit Enter, and it just doesn't register, with no explanation anywhere on screen.

The footer (`renderFooter`) is static and minimal: `/help  /model  /sessions` on the left, always — three of the twelve real commands, hard-coded, not context-sensitive. The right side shows `Alt+↵ newline · ↵ send` when idle, `Esc cancel · working…` when busy, and token counts *only* after a turn completes and only if `inputTokens`/`outputTokens` are already nonzero. There's no live token/context-window indicator, no cost estimate, and no elapsed-time readout while a turn is running — you get a static "working…" with no sense of whether it's been two seconds or two minutes.

Tool execution appears in the transcript as a status glyph (`…` → `✓`/`✗`) plus a byte count once the tool finishes; there's no live "here's what it's doing right now" feed the way hermes-agent streams `terminal \`ls -la\` (0.3s)` as it happens.

There's no first-run wizard analogous to `hermes setup --portal` or `openclaw onboard`. The documented path (`CLAUDE.md`, `config.example.toml`) is: `waffle secret init`, then pipe a key into `waffle secret set anthropic/api-key`, then hand-edit `config.toml` for agent profiles, then separately run `waffle provider add` for model discovery. Each individual step is well-designed (the provider flow in particular is genuinely nice — probes the upstream, commits credential and config transactionally), but they're disconnected commands you have to already know exist, with no single entry point that walks you through all of them in order.

Command discovery outside of typing `/` cold is thin: the palette (`renderPalette`) only appears once you've already typed `/`, and it shows raw `Usage` strings with no description alongside — you'd need `/help` open in a second overlay to see what each one does. hermes-agent's autocomplete dropdown and hermes/OpenClaw's auto-registration of skills as their own top-level commands (rather than requiring `/skill name`) both lower that floor further.

## Where Waffle is already ahead, and shouldn't chase the others

It's worth saying plainly: NanoClaw has no dashboard and reviewers call that a real weakness, hermes-agent's "closed learning loop" is explicitly flagged by reviewers as capable of accumulating "surprising behaviours" and skills "not always the ones a human would design," and OpenClaw's security model is architecturally weaker than Waffle's (process-level allowlists vs. Waffle's tighten-only profile hierarchy with sandboxed non-main tiers by default). Waffle's `waffle learn` → inactive proposal → `waffle skills activate` pipeline is a more disciplined, human-gated version of the same idea hermes-agent ships autonomously — that's a feature to surface more visibly in the UI, not a gap to close by copying their approach. The goal here isn't "add every feature the other three have," it's "make the features Waffle already has legible in the moment you're using them."

## What would move the needle, roughly in order of effort-to-payoff

Fixing the silent-Enter-during-turn bug is the highest-leverage single change — either implement a real busy-input mode (even just "queue" would remove the confusion) or, at minimum, emit a notice event so the composer visibly rejects the input instead of eating it.

Second: make the footer context-sensitive and add a live indicator during turns — token count and elapsed time cost nothing to compute (the runtime already tracks `inputTokens`/`outputTokens`) and would close most of the gap with hermes-agent's status bar without needing a cost model.

Third: a `waffle setup` (or `waffle chat` on an unconfigured install detecting it and offering) that chains `secret init` → provider add → a starter profile, so first run is one command instead of four you have to already know about.

Fourth, lower priority but cheap: show descriptions inline in the `/` palette instead of requiring a separate `/help` overlay, and consider auto-registering skill names as top-level slash commands the way hermes-agent does, since `internal/skill` already has everything needed (name, description) to do that.

None of this touches the backend, sandboxing, or trust model — it's entirely in `internal/chatui` and `internal/chat`, which is good news: the parts of Waffle that are hardest to get right are already the parts that are working.

Sources:
- [CLI Interface | Hermes Agent](https://hermes-agent.nousresearch.com/docs/user-guide/cli)
- [Slash Commands Reference | Hermes Agent](https://hermes-agent.nousresearch.com/docs/reference/slash-commands)
- [Personal assistant setup · OpenClaw](https://docs.openclaw.ai/start/openclaw)
- [OpenClaw TUI docs](https://openclawcn.com/en/docs/web/tui/)
- [NanoClaw — Secure AI Agent for WhatsApp, Telegram & More](https://nanoclaw.dev/)
- [NanoClaw's answer to OpenClaw is minimal code, maximum isolation - The New Stack](https://thenewstack.io/nanoclaw-minimalist-ai-agents/)
- [OpenClaw vs NanoClaw 2026 — Technerdo](https://www.technerdo.com/blog/openclaw-vs-nanoclaw-2026)
- [OpenClaw vs Hermes Agent 2026 — Technerdo](https://www.technerdo.com/blog/openclaw-vs-hermes-agent-2026)
