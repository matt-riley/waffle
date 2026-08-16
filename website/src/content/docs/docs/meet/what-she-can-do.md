---
title: What she can do
description: Conversation, tools, memory, and scheduled work — described in terms of what you get, not how it is built.
sidebar:
  order: 2
---

Four things, really. Everything else is a variation on one of them.

## Talk with you

The main event. Waffle holds a conversation in your terminal, remembers what
you said earlier in it, and can pick a session back up days later.

She is good at the things a knowledgeable colleague is good at: explaining
something confusing, drafting a first version, talking through a decision,
noticing what you forgot. She is not an oracle, and she will occasionally be
confidently wrong — the same as any other assistant of this kind.

## Do things with tools

This is the part that separates her from a chat window. When you allow it, she
can:

- **Read and write files** — open a file, explain it, edit it, create one.
- **Run commands** — anything you would type at a shell, subject to the rules
  you have set.
- **Search** — across your files, or the web.
- **Fetch a page** — pull something down and work with it.

Each of those is a permission you grant. She starts with none of them enabled
by default, and [Keeping her safe](/docs/meet/keeping-her-safe/) covers how to
decide what to switch on.

## Remember

Three different kinds of remembering, which sound similar and are not:

- **The conversation** — everything said in the current session, searchable
  later.
- **The working set** — goals and constraints she is holding for *this* task.
  Temporary by design; it disappears with the session.
- **Durable notes** — things worth keeping. These survive everything and are
  the only kind she carries into a new conversation.

Only that last kind is permanent, and there is a review setting that makes you
approve every durable note before it is written. [Teaching
her](/docs/meet/teaching-her/) covers all three.

## Work while you are not there

Waffle can run a job on a schedule — every weekday at nine, every night at
three — and message you with the result:

```sh
waffle cron add standup 0 9 * * 1-5 "Summarize my starred repos" \
  --deliver telegram:900
```

Scheduled jobs only run while the background service (`waffle serve`) is
running. They also run under a more restricted profile than your interactive
chat, because nobody is watching them.

## What she cannot do

- **Act without being asked.** She responds to your messages and to schedules
  you created. She does not decide on her own to go and do something.
- **Reach the internet from a sandbox** unless you allowed specific hosts.
- **Serve anyone but you.** Someone else messaging her gets a pairing code and
  nothing more.
- **Remember what you deleted.** `waffle forget <query>` previews matches and
  removes the ones you confirm.

---

**Nerd corner:** [How it fits together](/docs/under-the-hood/architecture/) —
the tool registry, the three memory layers, and how scheduled work is isolated.
