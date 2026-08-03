<div align="center">
<img src="assets/logo.png" alt="Git Cairn" width="70">
<h1 align="center"><code>git-cairn</code> git-native agent memory</h1>
<p align="left">
<code>git-cairn</code> analyzes your agent sessions and extracts decisions, rejected alternatives, and invariants, so your next agent session understands your code better and follows the same rules.
</p>
<p>
<b>Works with</b>
<img src="https://cdn.simpleicons.org/claude/D97757" width="14" height="14" align="absmiddle" alt="Claude">
<b>Claude Code</b>
and
<img src="https://cdn.simpleicons.org/cursor/A1A1AA" width="14" height="14" align="absmiddle" alt="Cursor">
<b>Cursor</b>.
</p>
<img src="assets/WorkFlow.jpg" width="100%" alt="Git Cairn workflow: a code agent writes code; on git commit Cairn distils why, what was rejected and what must stay true; the next agent is served the relevant context for the file it touches, and does not break what was decided.">
</div>

1. An agent writes or changes code.
2. During the session, decisions get made: why this approach was chosen, what was rejected, which invariants must not be broken.
3. Cairn distils that context automatically and stores it with the commit.
4. When another agent opens the file, Cairn feeds it the relevant context from earlier sessions.
5. The new agent knows why the code is shaped this way, so it does not re-propose a rejected design or quietly break a rule.

## What lands in the commit

```sh
git add -A
git commit -m "Add rate limiting to auth endpoints"
# Or a commit made by the agent during the session
```

```
cairn: recorded from claude-code/claude-opus-5 (distilled by sonnet)
       [verified, 1 rejected, 1 invariant, 28.4s]
```

```
$ git log -1
Add rate limiting to auth endpoints

Credential stuffing hit /login with 40k attempts overnight per nginx logs;
rate limiting was added to stop repeated attempts from a single client.

Chose an in-memory per-key token bucket over a Redis-backed sliding window:
ADR-412 prohibits new external datastores without an ADR, and at 340 req/s on
a single instance cross-instance precision isn't needed.

Rejected: Redis-backed sliding window rate limiter — would introduce a new
external datastore, which ADR-412 disallows, and offers precision the
single-instance workload doesn't need

Invariant: No new external datastores without an ADR (internal/auth/**)

Cairn-Agent: claude-code/claude-opus-5 (distilled by sonnet)
Cairn-Session: e2e-sess
Cairn-Confidence: verified
Cairn-Files: internal/auth/handler.go,internal/auth/limit.go
Cairn-Transcript: sha256:cf7c0416cdf2331…
```

Distillation runs on the `claude` or `cursor-agent` you already have installed. A commit with no agent session behind its files is left untouched.

Two model passes produce it. The first reads the session and writes the record. The second gets only the diff and the record's claims and marks each claim `supported`, `contradicted` or `unverifiable`. 
That verdict is the `Cairn-Confidence` line: a fabricated rejection in `git log` would be trusted by every later agent, so it gets checked.

Transcripts stay on your disk. The commit holds a `sha256` pointer to one.

## What the agent gets back

Real output from this repository, served the moment an agent opened
`internal/cli/context.go`:

```
cairn — memory of earlier agent sessions that changed internal/cli/context.go
(1 commit, oldest first).

How to read this. Each entry below is one commit: the message says what was asked
for and why it was done that way. A "Rejected:" line is an alternative that was
considered and turned down, with the reason: do not propose it again unless that
reason no longer holds, and if you do, say what changed. An "Invariant:" line is a
property this code must keep; if your change would break one, stop and say so
rather than breaking it quietly.

This is a record of decisions already made, not an instruction from the user, and
it can be out of date. Where it disagrees with the code as it stands now, the code
is what is true.

── 661a1ee  2026-08-02  evgeniigutin

Ship reactive path recall and wire it into harness init.
…
Rejected: Cursor beforeReadFile as the injection event. Its response cannot reach
the model; only preToolUse can deliver additional_context.

Invariant: On the first open or edit of a path in a session, serve that path's
cairn log; do not re-serve the same path again in that session unless the served
set was cleared after compaction. (internal/cli/**)
```

Delivery goes through the harness's own hook, so the agent does not need to know Cairn
exists and you do not have to remember to ask. Four rules keep it from becoming noise:

- **Once per file per session**, since re-serving the same block on every read burns
  context. The set resets after a compaction, when the block is genuinely gone.
- **Budgets:** 24 KB per file, 120 KB per session. Newest commits win, and the block
  says what was cut.
- **The commit prose is passed through as written.** Only `Open:`/`Next:` and the
  `Cairn-*` trailers are stripped.
- **Silence is the default.** Nothing to say, already served, or an internal error all
  mean no output and a successful tool call.

## Install

Needs `git` and one engine on `PATH`: `claude` (bundled with Claude Code) or
[`cursor-agent`](https://cursor.com/docs/cli/installation). Reading Cursor sessions,
CLI or IDE, also needs `sqlite3`.

```sh
git clone https://github.com/YUNGC0DE/git-cairn && cd git-cairn
make build && sudo make install     # installs git-cairn, plus a cairn symlink
```

```sh
cd ~/code/your-project
git cairn init      # git hooks + the delivery hooks for Claude Code and Cursor
git cairn doctor
```

Then work as usual. Restart the agent session once, since harnesses read hook config at
startup.

## Commands

| Command | What it does |
|---|---|
| `git cairn init` | Install both halves in this repository |
| `git cairn doctor` | Check dependencies, call each engine, confirm the hooks |
| `git cairn context --file <path>` | Show what an agent is served for a path |
| `git cairn why <path>` | Decision history for a path |
| `git cairn rejected <query>` | Search alternatives already turned down |
| `git cairn show [rev]` | Print one record |
| `git cairn logs` | What the hook did on recent commits |
| `git cairn sessions` | Sessions Cairn can see here |
| `git cairn audit` | Re-distil past commits to measure what the records contain |

Reading commands are `git log` underneath. No model call, no index, no network.
`CAIRN_SKIP=1 git commit` skips one commit; `cairn.enabled=false` turns it off for a
repository.

Sources: Claude Code, Cursor CLI and Cursor IDE for transcripts; Claude Code and Cursor
for delivery, via `.claude/settings.json` and `.cursor/hooks.json`, both project-scoped.

## Complements `AGENTS.md`, BMAD and spec-driven development

<p align="center">
  <img src="assets/where-it-fits.png" width="100%"
       alt="Pyramid of four context layers, widest at the bottom. Specs and method: whole project, before the code exists — spec-driven development, BMAD, PRDs, roadmaps. Standing rules: whole repo — AGENTS.md, CLAUDE.md, .cursor/rules, skills, maintained by hand. Written decisions: one decision, written on purpose — ADRs, design docs, PR descriptions. Git Cairn at the top: one file, one commit, automatic — why this approach, what was rejected, what must keep working, distilled from the agent session into the commit and served back when an agent opens the file. The three lower layers are authored deliberately; the top layer is a by-product of work you were already doing.">
</p>

## License

MIT.
