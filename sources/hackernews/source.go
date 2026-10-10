// Package hackernews fetches popular Ask/Show/Tell HN posts from the Algolia
// HN Search API and (best-effort) annotates each with a one-line summary and a
// comment-thread sentiment read.
package hackernews

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/0x7461/guild/bot"
	"github.com/0x7461/guild/bot/curate"
)

// Section is one HN category to pull (tag) and how many to keep.
type Section struct {
	Tag   string // Algolia tag: "ask_hn", "show_hn", "tell_hn"
	Label string // display label: "Ask HN", "Show HN", "Tell HN"
	Count int    // how many top posts to keep
}

// DefaultSections is the standard Ask/Show/Tell mix.
var DefaultSections = []Section{
	{Tag: "ask_hn", Label: "Ask HN", Count: 6},
	{Tag: "show_hn", Label: "Show HN", Count: 6},
	{Tag: "tell_hn", Label: "Tell HN", Count: 4},
}

// Source fetches popular HN posts, ranked by points + comments.
type Source struct {
	Sections   []Section
	Days       int  // lookback window (default 7)
	Annotate   bool // attach summary + sentiment via LLM
	Backend    string
	Model      string
	TimeoutSec int
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func (s *Source) Fetch() ([]bot.Item, error) {
	sections := s.Sections
	if len(sections) == 0 {
		sections = DefaultSections
	}
	days := s.Days
	if days <= 0 {
		days = 7
	}
	since := time.Now().AddDate(0, 0, -days).Unix()

	var items []bot.Item
	for _, sec := range sections {
		hits, err := search(sec.Tag, since)
		if err != nil {
			// Non-fatal: skip this section, keep the rest.
			fmt.Printf("hackernews: skipping %s: %v\n", sec.Tag, err)
			continue
		}
		// Re-rank client-side by points + comments, then keep top Count.
		sort.SliceStable(hits, func(i, j int) bool {
			return hits[i].score() > hits[j].score()
		})
		if len(hits) > sec.Count {
			hits = hits[:sec.Count]
		}
		for _, h := range hits {
			items = append(items, bot.Item{
				Title: h.Title,
				URL:   "https://news.ycombinator.com/item?id=" + h.ObjectID,
				Meta: map[string]string{
					"hn_type":  sec.Label,
					"points":   fmt.Sprintf("%d", h.Points),
					"comments": fmt.Sprintf("%d", h.NumComments),
				},
			})
		}
	}

	if s.Annotate && len(items) > 0 {
		s.annotate(items)
	}
	return items, nil
}

// annotate fetches top comments for each item and asks an LLM for a one-line
// summary + thread sentiment. Best-effort: failures leave items unchanged.
func (s *Source) annotate(items []bot.Item) {
	for i := range items {
		id := idFromURL(items[i].URL)
		if id == "" {
			continue
		}
		if ctx := topLevelComments(id, 6, 300); ctx != "" {
			items[i].Meta["context"] = ctx
		}
	}
	timeout := time.Duration(s.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	curate.Annotate(items, s.Backend, s.Model, timeout)
}

// --- Algolia search ---

type hit struct {
	Title       string `json:"title"`
	ObjectID    string `json:"objectID"`
	Points      int    `json:"points"`
	NumComments int    `json:"num_comments"`
}

func (h hit) score() int { return h.Points + h.NumComments }

func search(tag string, since int64) ([]hit, error) {
	q := url.Values{}
	q.Set("tags", tag)
	q.Set("numericFilters", fmt.Sprintf("created_at_i>%d", since))
	q.Set("hitsPerPage", "40")
	resp, err := httpClient.Get("https://hn.algolia.com/api/v1/search?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Hits []hit `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Hits, nil
}

// --- comment fetch ---

type itemNode struct {
	Text     string     `json:"text"`
	Points   int        `json:"points"`
	Children []itemNode `json:"children"`
}

// topLevelComments returns the first n top-level comments, each stripped and
// truncated to maxChars, joined into a single context blob. HN's Algolia API
// gives comment nodes no score, so a "top" ranking isn't available — the old
// sort-by-points was a no-op that just took thread order.
func topLevelComments(id string, n, maxChars int) string {
	resp, err := httpClient.Get("https://hn.algolia.com/api/v1/items/" + id)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var root itemNode
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return ""
	}
	kids := make([]itemNode, 0, len(root.Children))
	for _, c := range root.Children {
		if strings.TrimSpace(c.Text) != "" {
			kids = append(kids, c)
		}
	}
	if len(kids) > n {
		kids = kids[:n]
	}
	var parts []string
	for _, c := range kids {
		t := stripHTML(c.Text)
		if len(t) > maxChars {
			t = strings.TrimSpace(t[:maxChars]) + "…"
		}
		parts = append(parts, "- "+t)
	}
	return strings.Join(parts, "\n")
}

var (
	tagRE    = regexp.MustCompile(`<[^>]*>`)
	idRE     = regexp.MustCompile(`id=(\d+)`)
	entities = strings.NewReplacer(
		"&#x27;", "'", "&#x2F;", "/", "&quot;", `"`,
		"&gt;", ">", "&lt;", "<", "&amp;", "&",
	)
)

func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<p>", "\n")
	s = tagRE.ReplaceAllString(s, " ")
	s = entities.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

func idFromURL(u string) string {
	if m := idRE.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}
