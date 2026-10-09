# AGENTS.md — guild

Updated: 2026-10-09

Lightweight Go framework for scheduled Telegram bots. Three interfaces (Source / Formatter / Sender) wired into one runner; each bot is its own binary on a runit + snooze schedule. Binaries: paperboy (RSS digest), scout (combined GitHub trending + HN Ask/Show/Tell — `bot.MultiSource`), and nagger (daily Claude-quota pace nudge + monthly DeepSeek spend budget + recurring manual-task reminders — uses `senders/telegram` directly rather than the Source/Formatter runner).

> The `ai-agent` bot ("The Smartass" — interactive multi-backend Telegram chat) was **retired 2026-06-14** (superseded by Claude's remote-control; barely used). See `HISTORY.md`.

Audience: agents editing this repo. Framework overview + bot list in `README.md`. Decisions, internals, history in `PLAN.md` (local-only — gitignored).

## Setup

```bash
go mod download
cp .env.example .env   # umbrella (shared): ENABLE_TELEGRAM=true, TELEGRAM_*, GMAIL_*
# per-bot secrets go in .env.<name> (.env.gh, .env.paperboy, .env.nagger):
#   BOT_<NAME>__TOKEN / BOT_<NAME>__CHAT
```

Each bot loads `.env.<name>` then the umbrella `.env` via `bot.LoadEnv(name)` (first-wins: per-bot overrides shared). All `.env*` except `.env.example` are gitignored.

## Services

Declared runtime state — reconciled against `sv status` + `down` sentinels by `caretaker doctor` and the weekly caretaker scan (`service.claim_*` findings). `persistent` = must be up and survive reboot.

- `scout`: persistent, every 7d — weekly Sat 09:00 GitHub trending + HN digest (`snooze -w6 -H9`). Credentials in `.env.scout` (`BOT_SCOUT__TOKEN`/`BOT_SCOUT__CHAT`). Replaced `github-trending`/gh-bot (retired 2026-06-17).
- `paperboy`: persistent, every 1d — fires daily 18:00, **delivers Mon + Fri only** (`snooze -H18` → `-mode=deliver` on `date +%u` 1|5, else `-mode=curate`). The split is deliberate: a feed only exposes its newest `max_items`, so a twice-weekly *fetch* loses whatever the busy feeds published in between. Curating daily and queueing the winners (`pending` table in `rss-seen.db`) keeps every day's items and holds each curate prompt to ~50 items — the size gemma4 handles well — instead of one 3× longer on delivery day.
- `nagger`: persistent, every 12h — hourly 08–22 (`snooze -H8-22 ./bin/nagger`). Runs a unified set of periodic **nags** (`cmd/nagger`: `Nag` = id + interval + anchor + renderer): the daily Claude-quota pace check (interval 1, live-computed message), the **DeepSeek spend budget** (evaluated every run, fires once per crossed threshold per month) and recurring manual-task **reminders** (fixed-day cadence, e.g. quarterly archive chores; configured under `reminders` in `nagger.json`). One run evaluates all nags, fires the due ones as **one combined message** (grouped under headers), and dedups per-nag via `state.json`. Per-nag dedup makes the hourly poll idempotent (each nag fires once per cycle).

## Commands

```bash
# Build (run after every code change before sv restart)
go build -o bin/scout    ./cmd/scout/
go build -o bin/paperboy  ./cmd/paperboy/

# Dry-run scout without sending (umbrella .env sets ENABLE_TELEGRAM=true, so override):
ENABLE_TELEGRAM=false go run ./cmd/scout/

# Service control
SVDIR=~/service sv status scout
SVDIR=~/service sv restart paperboy

# Dry run (without ENABLE_TELEGRAM=true, bots log instead of POSTing)
go run ./cmd/paperboy/
```

## Project layout

```
bot/                              framework: Item, Source/Formatter/Sender, Bot runner, MultiSource
bot/curate/                       LLM passes: ranking (ChainCurator, paperboy), Summarize (paperboy) +
                                  Annotate (scout). Backends: Ollama or claude -p, chosen per pass by
                                  config and routed explicitly via runText/backendFor.
cmd/{paperboy,scout,nagger}/       bot entry points — one binary each
sources/{rss,github,hackernews}/  Source implementations (gofeed, goquery, Algolia HN API)
formatters/{rss,scout}/           Formatter implementations
senders/telegram/                 Telegram sender + GetChatID helper
config/                           per-bot JSON config loader (~/.config/guild/<bot>.json)
compress/                         summarize CLI shell-out (scout's GitHub trending summaries)
bin/                              built binaries (gitignored)
```

External integration points:
- `~/service/{scout,paperboy,nagger}/` — runit user services.
- `~/.local/share/guild/rss-seen.db` — RSS SQLite: `seen` (judged guids, 90d retention) + `pending` (curated-but-undelivered items, **stored whole** — a guid is useless later because the feed window has moved on; 14d retention bounds it if delivery stops firing).
- `~/.config/guild/<bot>.json` — per-bot config overrides (scout: period/summarize/limit + hn block; paperboy: feed list, max_delivery, `curate` block, `summarize` block — per-item one-line summaries (backend/model from the live config — currently `gemma4:e4b` on ollama), off unless `enabled`). **`paperboy.json` is chezmoi-managed** — `chezmoi re-add ~/.config/guild/paperboy.json` after editing it live, or the source drifts (scout/nagger json are not tracked).
- `~/.config/guild/nagger.json` — nagger schedule config + `reminders` array, read by `cmd/nagger`. Hand-edited (was written by ai-agent's `/nagger` before that bot's retirement). Each reminder: `{id, message, every_days, anchor}` (anchor = first due date when never fired). **`quota_enabled`** gates the daily Claude-quota nag; it defaults to `true` via `cmd/nagger`'s config literal, so an absent key keeps the nag. Set to `false` 2026-09-17 — Claude Pro is cancelled, paid through 2026-10-10, so there is no quota to pace. **`spend_quota_usd`** (monthly DeepSeek budget; 0/absent disables the spend nag; `~/.pi/agent/extensions/status.ts` reads the same key) and **`warn_fractions`** (default `[0.5, 0.8, 0.9]`; live config adds `1.0`). The balance comes from DeepSeek's `/user/balance` with the key at `~/.config/deepseek/key_void`; an unreadable balance fires a `spend-error` nag at most daily.
- `~/.local/share/nagger/{rate-limits.json,state.json}` — `rate-limits.json` is the pace cache (written by `~/.claude/statusline.py` every CC response — external, don't move; `statusline.sh` became a thin per-OS runner 2026-09-18 and no longer writes it). The writer skips the write where `~/.local/share` is absent, and skips it when no `rate_limits` field is present, so the last good reading survives a payload without limits. `state.json` holds `lastFired` (id→YYYY-MM-DD, incl. `quota`) and `spend`, the month ledger (`start_balance`, `topups`, `last_balance`, `fired` thresholds; config is immutable, state is separate). A rise in balance is booked as a top-up, never as negative spend, and the ledger restarts on the first run of a month. The flat id→date format (until 2026-09-29) is read as `lastFired`; the older split `last-sent` + `reminders-state.json` still migrates once if `state.json` is absent. **Spend is account-wide:** if the DeepSeek key is used on another machine, its spend counts here too.

## Boundaries & gotchas

**Always do:**
- **`MarkSeen` means *judged*, not *delivered*.** In the split curate/deliver flow those happen on different days; `pending` is what carries an item between them. Curate marks everything it fetched — including what it dropped — so a rejected item doesn't return tomorrow merely because it is still inside the feed window. `ClearPending` is the delivery-side counterpart and runs **only after a successful send**, so a failed send re-delivers rather than losing the digest. Don't "fix" the naming by moving `MarkSeen` back to post-send.
- **Set `ENABLE_TELEGRAM=true` in `.env`** for real sends; otherwise bots dry-run (log instead of POST). Required in prod.
- **`cd /path/to/project` before `exec` in runit `run` scripts.** runit doesn't set CWD; `godotenv.Load()` won't find `.env` without it.
- **scout's `run` exports `PATH="/home/ta/.local/share/mise/shims:/home/ta/.local/bin:$PATH"`.** `compress/` shells out to bare `summarize`, an npm CLI (`#!/usr/bin/env node`) — without mise's node shim on PATH it fails with `env: 'node': No such file or directory`. Don't rely on runsvdir's inherited PATH (it comes from whatever session started niri).
- **Rebuild AND restart after code changes:** `go build -o bin/<bot> ./cmd/<bot>/` AND `SVDIR=~/service sv restart <bot>`. runit runs the pre-built binary from `bin/`, not `go run`. Stale binaries silently serve old behavior — hit production 2026-03-13 (formatter rewritten 2026-03-09, binary still from 2026-03-07).
- **One binary per bot.** Different schedules, different tokens, different lifecycles. Don't bundle.
- **Use `BOT_<NAME>__TOKEN` / `BOT_<NAME>__CHAT`** for per-bot Telegram credentials; falls back to generic `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` if unset. Each bot ideally has its own BotFather token. Per-bot secrets live in `.env.<name>`, shared values in the umbrella `.env`; both loaded via `bot.LoadEnv(name)`.

**Never do:**
- **Don't merge bot binaries** *when they differ in schedule, token, .env scope, or lifecycle.* That's the test, not "always separate". Bots that share all four belong together via `bot.MultiSource` (one binary, many sources) — e.g. scout folds GitHub trending + HN into one weekly Sat digest. Don't split a single coherent digest into N binaries, and don't bundle bots with divergent cadence/tokens.
- **Don't use `cmd.Output()` for `claude -p` invocations.** When CC quota expires, `claude -p` writes the error to **stdout, not stderr**. `cmd.Output()` discards stdout on error → empty error message. Use an explicit `bytes.Buffer` for stderr and fall back to stdout content if stderr is empty. Applies to any `claude -p` shell-out (e.g. `bot/curate/`).
- **Keep GitHub trending (scout's `sources/github`) and RSS (paperboy) as separate sources on separate bots.** Don't fold RSS feeds into scout or GitHub trending into paperboy — different schedules, formatters, and lifecycles.
- **Don't pass `--bare` to `claude -p`** in `bot/curate/`. `--bare` skips not just CLAUDE.md/settings but also auth discovery → "Not logged in" failure. Caught 2026-05-25 when first wiring paperboy curation.
- **Don't lower `ollamaNumCtx` / `ollamaNumPredict` (`bot/curate/curate.go`) without re-measuring.** Both Ollama defaults fail *silently*, which is why they are pinned rather than left alone: a prompt longer than `num_ctx` (default 8192, 4096 on `/v1`) is **truncated with no error**, and hitting `num_predict` returns an **empty** `response` with `done_reason: "length"` — which reaches the caller as "no JSON array found", indistinguishable from a bad answer. `runOllamaText` turns the empty case into an explicit error naming `num_predict`; keep that. Measured 2026-09-24 on `gemma4:e4b`, CPU-only (no GPU offload — 9.6GB model against 4GB VRAM): 28s model load, 57 tok/s prefill, 8.5 tok/s generation. A 120-item curate prompt is ~9.7k tokens, so a cold run costs ~4.5 minutes — hence the 600s timeouts. The old 60s curate / 120s summarize caps would fail every cold run.

**Ask first:**
- Adding a new bot binary. Comes with runit service setup, BotFather token, schedule decision — discuss in PLAN.md `## Decisions` first.

**Untested / known-fragile:**
- Recovery path when `~/.config/guild/nagger.json` is malformed. `cmd/nagger` reads it; error paths on a bad hand-edit haven't been exercised.

## Where to look

- **`README.md`** — framework overview, bot list, interface signatures, dependencies.
- **`PLAN.md ## Decisions`** (local-only) — architectural choices: separate binaries, snooze+runit scheduling, three-interface split.
- **`PLAN.md ## Internals`** — recap integration, dotenv/runit interaction details.
- **`HISTORY.md`** — what shipped when, including the 2026-06-14 ai-agent retirement and the 2026-03-13 Opus code-review hardening (20 issues fixed in one commit, `b10018a`).
- **`cmd/nagger/`** — quota-pace nudge, folded in 2026-06-05 (was the standalone `~/projects/nagger` Python project). Reads `~/.config/guild/nagger.json`.
