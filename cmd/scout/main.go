package main

import (
	"fmt"
	"log"
	"os"

	"github.com/0x7461/guild/bot"
	"github.com/0x7461/guild/config"
	"github.com/0x7461/guild/formatters/scout"
	"github.com/0x7461/guild/senders/telegram"
	github "github.com/0x7461/guild/sources/github"
	"github.com/0x7461/guild/sources/hackernews"
)

func main() {
	bot.LoadEnv("scout")

	cfg := config.ScoutConfig{}
	cfg.GitHub.Period = "weekly"
	cfg.GitHub.Summarize = true
	cfg.GitHub.Limit = 10
	cfg.HN.Days = 7
	cfg.HN.Annotate = true
	cfg.HN.Backend = "openai"
	cfg.HN.Model = "deepseek-v4.1-flash"
	cfg.HN.TimeoutSec = 300
	cfg.Formatter.Title = "Scout — Weekly GitHub + HN"
	if err := config.Load("scout", &cfg); err != nil {
		log.Printf("Warning: could not load scout config: %v", err)
	}

	source := &bot.MultiSource{Sources: []bot.NamedSource{
		{Name: "GitHub Trending", Source: &github.TrendingSource{Period: cfg.GitHub.Period, Summarize: cfg.GitHub.Summarize, Limit: cfg.GitHub.Limit}},
		{Name: "Hacker News", Source: &hackernews.Source{
			Days:       cfg.HN.Days,
			Annotate:   cfg.HN.Annotate,
			Backend:    cfg.HN.Backend,
			Model:      cfg.HN.Model,
			TimeoutSec: cfg.HN.TimeoutSec,
		}},
	}}
	formatter := &scout.Formatter{Title: cfg.Formatter.Title}

	if os.Getenv("ENABLE_TELEGRAM") != "true" {
		// Dry run — fetch and print count only.
		items, err := source.Fetch()
		if err != nil {
			log.Fatalf("Error: %v", err)
		}
		fmt.Printf("Found %d items (Telegram disabled — set ENABLE_TELEGRAM=true to send)\n", len(items))
		fmt.Println(formatter.Format(items))
		return
	}

	token := bot.FirstNonEmpty(os.Getenv("BOT_SCOUT__TOKEN"), os.Getenv("TELEGRAM_BOT_TOKEN"))
	var chatID int64
	chatStr := bot.FirstNonEmpty(os.Getenv("BOT_SCOUT__CHAT"), os.Getenv("TELEGRAM_CHAT_ID"))
	if chatStr != "" {
		if _, err := fmt.Sscanf(chatStr, "%d", &chatID); err != nil {
			log.Fatalf("invalid chat ID %q: %v", chatStr, err)
		}
	}

	if token == "" || chatID == 0 {
		log.Fatal("ENABLE_TELEGRAM=true but BOT_SCOUT__TOKEN/BOT_SCOUT__CHAT (or TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID) is missing")
	}

	b := &bot.Bot{
		Source:    source,
		Formatter: formatter,
		Sender:    &telegram.Sender{Token: token, ChatID: chatID},
	}

	fmt.Println("Fetching scout digest (GitHub + HN)...")
	if err := b.Run(); err != nil {
		log.Fatalf("Bot error: %v", err)
	}
	fmt.Println("[OK] Done!")
}
