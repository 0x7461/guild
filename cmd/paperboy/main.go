package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/0x7461/guild/bot"
	"github.com/0x7461/guild/bot/curate"
	"github.com/0x7461/guild/config"
	rssformatter "github.com/0x7461/guild/formatters/rss"
	"github.com/0x7461/guild/senders/telegram"
	"github.com/0x7461/guild/sources/rss"
)

var defaultFeeds = []rss.FeedConfig{
	{Name: "HN Best", URL: "https://hnrss.org/best", MaxItems: 10, DiscussionLabel: "HN"},
	{Name: "Lobsters", URL: "https://lobste.rs/rss", MaxItems: 10},
	{Name: "Techmeme", URL: "https://techmeme.com/feed.xml", MaxItems: 10},
	{Name: "Dan Luu", URL: "https://danluu.com/atom.xml", MaxItems: 5},
	{Name: "Julia Evans", URL: "https://jvns.ca/atom.xml", MaxItems: 5},
}

// Fetching and delivering run on different days on purpose. A feed only exposes
// its newest `max_items`, so a twice-weekly *fetch* silently loses whatever the
// busy feeds published in between. Curating daily and queueing the winners keeps
// every day's items, and hands the curator a list the size it already handles
// well rather than one three times longer.
const (
	modeCurate  = "curate"  // fetch, rank, queue. Sends nothing.
	modeDeliver = "deliver" // do all of the above, then flush the queue.
)

func main() {
	mode := flag.String("mode", modeDeliver,
		"curate (fetch + rank + queue, no send) or deliver (curate, then send the queue)")
	flag.Parse()
	if *mode != modeCurate && *mode != modeDeliver {
		log.Fatalf("unknown -mode %q (want %q or %q)", *mode, modeCurate, modeDeliver)
	}

	bot.LoadEnv("paperboy")

	cfg := config.PaperboyConfig{}
	cfg.Source.MaxDelivery = 50
	if err := config.Load("paperboy", &cfg); err != nil {
		log.Printf("Warning: could not load paperboy config: %v", err)
	}
	// A non-positive target silently kept an empty selection (and panicked when
	// negative), while the caller still marked every item seen.
	if cfg.Source.Curate.Enabled && cfg.Source.Curate.Target < 1 {
		log.Fatalf("paperboy: source.curate.target must be >= 1 when curate is enabled (got %d)", cfg.Source.Curate.Target)
	}

	feeds := defaultFeeds
	if len(cfg.Source.Feeds) > 0 {
		feeds = make([]rss.FeedConfig, len(cfg.Source.Feeds))
		for i, f := range cfg.Source.Feeds {
			feeds[i] = rss.FeedConfig{
				Name:            f.Name,
				URL:             f.URL,
				MaxItems:        f.MaxItems,
				DiscussionLabel: f.DiscussionLabel,
				Favored:         f.Favored,
			}
		}
	}
	maxDelivery := cfg.Source.MaxDelivery

	// Deduplication DB
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot determine home directory: %v", err)
	}
	dbPath := filepath.Join(home, ".local", "share", "guild", "rss-seen.db")
	dedup, err := rss.NewDeduplicator(dbPath)
	if err != nil {
		log.Fatalf("dedup init: %v", err)
	}
	defer dedup.Close()

	// Fetch
	source := &rss.RSSSource{Feeds: feeds}
	items, err := source.Fetch()
	if err != nil {
		log.Fatalf("fetch: %v", err)
	}
	fetched := len(items)

	// Filter seen
	unseen, err := dedup.Filter(items)
	if err != nil {
		log.Fatalf("dedup filter: %v", err)
	}
	fmt.Printf("fetched %d, deduped to %d\n", fetched, len(unseen))

	// Every feed goes through the LLM ranker; favored feeds only get a small
	// edge inside it (★ in the prompt), never a bypass.
	final := unseen
	if cfg.Source.Curate.Enabled && len(unseen) > 0 {
		curator := buildCurator(cfg.Source.Curate)
		if curator != nil {
			ranked, err := curator.Curate(unseen, cfg.Source.Curate.Target)
			if err != nil {
				fmt.Printf("curate: all backends failed, passing through %d items: %v\n", len(unseen), err)
			} else {
				final = ranked
			}
		}
	}

	// Queue the winners, then mark *everything fetched* judged — including the
	// items curation dropped. They were considered; letting them back in tomorrow
	// only because they are still inside the feed window would re-run the same
	// decision on the same items every day.
	if len(unseen) > 0 {
		if err := dedup.Keep(final); err != nil {
			log.Fatalf("queue: %v", err)
		}
		if err := dedup.MarkSeen(unseen); err != nil {
			log.Fatalf("mark judged: %v", err)
		}
	}

	if *mode == modeCurate {
		queued, err := dedup.PendingCount()
		if err != nil {
			log.Fatalf("queue count: %v", err)
		}
		fmt.Printf("curate: kept %d of %d, %d queued for the next delivery\n",
			len(final), len(unseen), queued)
		return
	}

	// Deliver everything queued since the last send, not just today's picks.
	final, err = dedup.Pending()
	if err != nil {
		log.Fatalf("read queue: %v", err)
	}

	// Cap to avoid flooding after outage / first run / curation pass-through
	if len(final) > maxDelivery {
		final = final[:maxDelivery]
	}

	if len(final) == 0 {
		fmt.Println("Nothing queued — nothing to send.")
		return
	}

	// Per-item one-line summaries (best-effort; digest ships unchanged on failure).
	if cfg.Source.Summarize.Enabled {
		timeout := time.Duration(cfg.Source.Summarize.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 300 * time.Second
		}
		curate.Summarize(final, cfg.Source.Summarize.Backend, cfg.Source.Summarize.Model, timeout)
	}

	fmt.Printf("delivering: %d items\n", len(final))

	if os.Getenv("ENABLE_TELEGRAM") != "true" {
		formatter := &rssformatter.Formatter{}
		for _, msg := range formatter.FormatAll(final) {
			fmt.Println(msg)
			fmt.Println("———")
		}
		fmt.Println("(Telegram disabled — set ENABLE_TELEGRAM=true to send)")
		return
	}

	token := bot.FirstNonEmpty(os.Getenv("BOT_PAPERBOY__TOKEN"), os.Getenv("TELEGRAM_BOT_TOKEN"))
	var chatID int64
	chatStr := bot.FirstNonEmpty(os.Getenv("BOT_PAPERBOY__CHAT"), os.Getenv("TELEGRAM_CHAT_ID"))
	if chatStr != "" {
		if _, err := fmt.Sscanf(chatStr, "%d", &chatID); err != nil {
			log.Fatalf("invalid chat ID %q: %v", chatStr, err)
		}
	}
	if token == "" || chatID == 0 {
		log.Fatal("ENABLE_TELEGRAM=true but BOT_PAPERBOY__TOKEN/BOT_PAPERBOY__CHAT (or TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID) is missing")
	}

	formatter := &rssformatter.Formatter{}
	sender := &telegram.Sender{Token: token, ChatID: chatID}

	messages := formatter.FormatAll(final)
	for _, msg := range messages {
		if err := sender.Send(msg); err != nil {
			log.Fatalf("send: %v", err)
		}
	}

	// Clear the queue only after a successful send. Anything still queued is
	// re-delivered next run, which is exactly what a failed send needs.
	if err := dedup.ClearPending(final); err != nil {
		log.Printf("warning: failed to clear delivered items from the queue: %v", err)
	}

	fmt.Printf("[OK] Delivered %d items in %d messages.\n", len(final), len(messages))
}

func buildCurator(cc config.CurateConfig) curate.Curator {
	timeout := time.Duration(cc.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		// Cold Ollama runs cost ~4.5 min (AGENTS.md) — the old 60s cap failed
		// every one of them.
		timeout = 600 * time.Second
	}
	primary := backendFor(cc.Backend, cc.Model, timeout)
	fallback := backendFor(cc.FallbackBackend, cc.FallbackModel, timeout)
	switch {
	case primary == nil && fallback == nil:
		fmt.Println("curate: no backend configured, skipping")
		return nil
	case fallback == nil:
		return &curate.ChainCurator{Curators: []curate.Curator{primary}}
	case primary == nil:
		return &curate.ChainCurator{Curators: []curate.Curator{fallback}}
	}
	return &curate.ChainCurator{Curators: []curate.Curator{primary, fallback}}
}

func backendFor(name, model string, timeout time.Duration) curate.Curator {
	if name == "" || model == "" {
		return nil
	}
	switch name {
	case "claude-code":
		return &curate.ClaudeCodeCurator{Model: model, Timeout: timeout}
	case "ollama":
		return &curate.OllamaCurator{Model: model, Timeout: timeout}
	case "openai", "opencode":
		// OpenAI-compatible lane: OpenCode Go by default. The key lives in guild's .env
		// (OPENCODE_API_KEY), which is gitignored — never in paperboy.json.
		return &curate.OpenAICurator{
			Model:   model,
			APIKey:  os.Getenv("OPENCODE_API_KEY"),
			Timeout: timeout,
		}
	default:
		fmt.Printf("curate: unknown backend %q, skipping\n", name)
		return nil
	}
}
