package rss

import (
	"fmt"
	"strings"

	"github.com/0x7461/guild/bot"
	"github.com/mmcdole/gofeed"
)

// FeedConfig describes a single RSS/Atom feed to follow.
type FeedConfig struct {
	Name            string
	URL             string
	MaxItems        int    // max items to return per fetch (default 5)
	DiscussionLabel string // label for discussion link when guid != link (e.g. "HN")
	Favored         bool   // if true, ranked with a small edge — still subject to LLM ranking
}

// RSSSource fetches items from a list of RSS/Atom feeds.
type RSSSource struct {
	Feeds []FeedConfig
}

func (s *RSSSource) Fetch() ([]bot.Item, error) {
	parser := gofeed.NewParser()
	var items []bot.Item

	for _, feed := range s.Feeds {
		max := feed.MaxItems
		if max <= 0 {
			max = 5
		}

		parsed, err := parser.ParseURL(feed.URL)
		if err != nil {
			// Non-fatal: log and continue with remaining feeds
			fmt.Printf("rss: skipping %s: %v\n", feed.Name, err)
			continue
		}

		for i, entry := range parsed.Items {
			if i >= max {
				break
			}

			guid := entry.GUID
			if guid == "" {
				guid = entry.Link
			}

			desc := entry.Description
			if desc == "" {
				desc = entry.Content
			}
			// Trim description to a reasonable length
			if len(desc) > 200 {
				desc = desc[:200] + "…"
			}

			meta := map[string]string{
				"feed": feed.Name,
				"guid": guid,
			}
			if feed.Favored {
				meta["favored"] = "true"
			}
			if guid != entry.Link && strings.HasPrefix(guid, "http") {
				label := feed.DiscussionLabel
				if label == "" {
					label = "Discussion"
				}
				meta["discussion"] = guid
				meta["discussion_label"] = label
			}

			items = append(items, bot.Item{
				Title:       entry.Title,
				URL:         entry.Link,
				Description: desc,
				Meta:        meta,
			})
		}
	}

	return items, nil
}
