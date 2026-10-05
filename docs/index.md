---
title: Ignatius
nav_order: 1
permalink: /
---

# Ignatius
{: .no_toc }

Ignatius is a gateway that sits in front of decision models like Jev, Clef and Decis. You send it one set of questions. It returns normalized answers from the models you picked. You host it yourself.

Decision models come in two kinds: fast and cheap, or slow and smart. You want both, cheap for the easy questions, smart for the hard ones. Every provider answers a little differently, so you end up writing glue code. Ignatius is that glue, and it keeps a trace of every decision it makes.

![The Ignatius status page](images/status-page.png)

*The status page: models, profiles, routes and every recent request. Demo data from two fake models, so the numbers mean nothing.*


## Three modes

1. **Single.** Send the questions to one model. It normalizes the answer.
2. **Fan-out.** Send the same questions to several models at once. Get every answer back, or combine them by vote, mean or most confident.
3. **Cascade.** Ask the cheap model first. Check how sure it is. Send only the questions it was unsure about to the next tier.

The saving comes from the cascade. If the cheap tier is confident on most questions, the expensive model only sees the hard ones.

## Where to go next

1. [Getting started](getting-started.html): build it, run it, make a call.
2. [Auth](auth.html): keys, clients, users and login.
3. [Routes and cascades](routes.html): how a plan is picked and how thresholds work.
4. [Self-hosting models](self-hosting.html): the dunce-union kit.
5. [Judging routes](eval.html): test a cascade on your own labeled data.

The full contract is in [SPEC.md](https://github.com/phin-tech/ignatius/blob/main/SPEC.md).
