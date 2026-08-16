---
title: What Waffle is
description: A plain-language introduction to Waffle — what she does, what she deliberately does not do, and who she is for.
sidebar:
  order: 1
---

Waffle is a personal AI assistant that runs on a computer you own.

You talk to her, she answers, and — when you let her — she does things: reads a
file, runs a command, searches the web, remembers something for next time. She
is one program on one machine, working for one person. That person is you.

She is named after a ginger kitten who is, by all accounts, a menace.

## What she can do

**Hold a conversation.** The main way in is a chat window in your terminal. Ask
her things, think out loud at her, paste in something confusing and ask what it
means.

**Use tools, when allowed.** Waffle can read and write files, run commands,
search your files, and fetch things from the web. Every one of those is a
permission you grant, not a default she starts with.

**Remember.** She keeps a transcript of your conversations, and she keeps
durable notes that survive between them. You can tell her to remember something,
and you can go and read — or delete — what she has written down.

**Do things on a schedule.** She can run a job every morning without you asking,
and message you if it turns up something worth knowing.

## What she deliberately is not

Waffle serves **exactly one owner**. She is not a team assistant, not a group
chatbot, and not a service you sign other people up for. If somebody else
messages her, they get a pairing code and nothing else.

She has **no cloud account of her own**. She runs on your hardware. Your
conversations and notes are files on your disk, in a database you can open.

She is **not a plugin marketplace**. What she can do is what you have configured
her to do, and the list is meant to stay short enough to hold in your head.

## Where she lives

Everything Waffle knows lives in one directory, `~/.waffle` by default: her
database, her notes, her configuration, and her encrypted secrets. Back that up
and you have backed up Waffle. Delete it and she is gone.

## Getting to hello

The short version, on a machine that already has the Go toolchain:

```sh
go install github.com/matt-riley/waffle/cmd/waffle@latest
waffle setup
waffle chat
```

`waffle setup` walks you through the parts that need decisions: creating her
encrypted store, connecting her to a model provider, and picking which model she
answers with. Then `waffle chat` opens the conversation.

If `waffle setup` finished but she cannot answer, she is **Installed** but not
yet **Ready** — she exists, but has no working model to think with. That is
almost always a provider that needs connecting, and `waffle doctor` will say so.

## Next

[What she can do](/docs/meet/what-she-can-do/), in more detail than the summary
above.

One page out of order, though: read [Keeping her
safe](/docs/meet/keeping-her-safe/) before you give her anything interesting to
do. It is short, and it is the page that matters most.

---

**Nerd corner:** [How it fits together](/docs/under-the-hood/architecture/) —
the binary, the agent loop, the trust boundaries, and where each piece of state
actually lands.
