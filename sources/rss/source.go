package rss

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

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
	failed := 0

	for _, feed := range s.Feeds {
		max := feed.MaxItems
		if max <= 0 {
			max = 5
		}

		parsed, err := fetchFeed(parser, feed.URL)
		if err != nil {
			// Non-fatal: log and continue with remaining feeds
			failed++
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
			// Trim description to a reasonable length, on a rune boundary — a byte
			// slice through a multibyte rune produced invalid UTF-8.
			if runes := []rune(desc); len(runes) > 200 {
				desc = string(runes[:200]) + "…"
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

	// One flaky feed must not sink the fetch, but every feed failing is not a
	// quiet day: paperboy's caller treats a fetch error as the run failing, so
	// the finish hook records 1 and caretaker reports it — `fetched 0, deduped
	// to 0` used to read the same whether feeds failed or nothing was new.
	if failed > 0 && failed == len(s.Feeds) {
		return nil, fmt.Errorf("every feed failed (%d of %d) — nothing fetched", failed, len(s.Feeds))
	}
	return items, nil
}

// fetchFeed fetches one feed, retrying transient network failures with backoff
// (3 attempts, 5s then 20s). A short DNS outage otherwise empties the whole
// window: 2026-10-24 paperboy passed wait-net, lost all 13 feeds to `network
// is unreachable` over four minutes and queued nothing. Permanent errors (bad
// HTTP status, parse failures) skip immediately — retrying them burns 25s and
// cannot succeed.
func fetchFeed(parser *gofeed.Parser, feedURL string) (*gofeed.Feed, error) {
	return fetchFeedWithRetry(parser, feedURL, []time.Duration{5 * time.Second, 20 * time.Second})
}

func fetchFeedWithRetry(parser *gofeed.Parser, feedURL string, delays []time.Duration) (*gofeed.Feed, error) {
	var err error
	for attempt := 0; attempt <= len(delays); attempt++ {
		parsed, ferr := parser.ParseURL(feedURL)
		if ferr == nil {
			return parsed, nil
		}
		err = ferr
		if !transientNetworkError(ferr) || attempt == len(delays) {
			return nil, err
		}
		fmt.Printf("rss: %s: transient failure (attempt %d of %d): %v — retrying in %s\n",
			feedURL, attempt+1, len(delays)+1, ferr, delays[attempt])
		time.Sleep(delays[attempt])
	}
	return nil, err
}

// transientNetworkError reports whether err is a network-level failure worth
// retrying (DNS, connection, timeout) as opposed to a server verdict
// (gofeed.HTTPError) or a content problem (decode, feed-type detection).
func transientNetworkError(err error) bool {
	var httpErr gofeed.HTTPError
	if errors.As(err, &httpErr) {
		return false
	}
	var uerr *url.Error
	return errors.As(err, &uerr)
}
