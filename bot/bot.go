package bot

import (
	"fmt"
	"strings"

	"github.com/joho/godotenv"
)

// MessageBreak is a formatter-emitted sentinel marking a hard message boundary:
// a Sender should start a new message here rather than only splitting on length.
// Senders without multi-message output may strip it. The byte sequence is chosen
// not to occur in normal digest text.
const MessageBreak = "\x00\x00BREAK\x00\x00"

// Item is a generic piece of content returned by a Source.
type Item struct {
	Title       string
	URL         string
	Description string
	Meta        map[string]string // flexible key-value for extras (stars, language, etc.)
}

// Source fetches items from an external data source.
type Source interface {
	Fetch() ([]Item, error)
}

// Formatter formats a list of items into a message string.
type Formatter interface {
	Format(items []Item) string
}

// Sender delivers a formatted message to a destination.
type Sender interface {
	Send(message string) error
}

// Bot wires a Source, Formatter, and Sender together.
type Bot struct {
	Source    Source
	Formatter Formatter
	Sender    Sender
}

// LoadEnv loads the per-bot then the umbrella .env from the working directory,
// tolerating missing files. First-wins: values in .env.<name> take precedence
// over the shared .env (godotenv never overrides an already-set var).
func LoadEnv(name string) {
	for _, f := range []string{".env." + name, ".env"} {
		_ = godotenv.Load(f)
	}
}

// FirstNonEmpty returns the first non-empty string from the arguments.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Run fetches, formats, and sends.
func (b *Bot) Run() error {
	items, err := b.Source.Fetch()
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	message := b.Formatter.Format(items)
	if strings.TrimSpace(message) == "" {
		return nil // nothing to send — don't emit a header-only message
	}
	if err := b.Sender.Send(message); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}
