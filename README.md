# guild

A lightweight Go framework for scheduled Telegram bots. Implement three interfaces — Source, Formatter, Sender — wire them together, schedule with snooze + runit.

## Framework

```go
type Source interface {
    Fetch() ([]Item, error)
}

type Formatter interface {
    Format(items []Item) string
}

type Sender interface {
    Send(message string) error
}
```

Adding a new bot = implement `Source`, pick a `Formatter` and `Sender`, pass to `bot.Bot{}`.

## Bots

- **paperboy** — RSS feed aggregator. HN Best, Lobsters, Techmeme, blogs. SQLite dedup. Curates daily, delivers Mon + Fri.
- **scout** — combined weekly "what's new online" digest: GitHub trending + HN Ask/Show/Tell, ranked by points+comments, each with a one-line summary and a comment-thread sentiment read. Built on `MultiSource`. Runs weekly.
- **nagger** — monthly DeepSeek spend budget + recurring manual-task reminders. Hourly 08–22, dedup'd per nag.

## Project Structure

```
bot/                        — framework: interfaces + Bot runner + MultiSource
cmd/{paperboy,scout,nagger}/ — bot entry points
sources/{rss,github,hackernews}/ — Source implementations
formatters/{rss,scout}/ — Formatter implementations
senders/telegram/           — Telegram sender (HTML, message splitting)
```

## Configuration

Copy `.env.example` to `.env` and fill in bot tokens and chat IDs. Each bot can have its own token (`BOT_PAPERBOY__TOKEN`, `BOT_SCOUT__TOKEN`) or fall back to `TELEGRAM_BOT_TOKEN`.

## Dependencies

- [goquery](https://github.com/PuerkitoBio/goquery) — HTML parsing (GitHub trending)
- [gofeed](https://github.com/mmcdole/gofeed) — RSS/Atom feed parsing
- [godotenv](https://github.com/joho/godotenv) — .env loading
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — SQLite (RSS dedup)
