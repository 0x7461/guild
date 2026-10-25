package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0x7461/guild/bot"
)

var senderHTTPClient = &http.Client{Timeout: 30 * time.Second}

// Sender delivers messages via the Telegram Bot API.
type Sender struct {
	Token  string
	ChatID int64
}

const telegramMaxLen = 4096
const repoSeparator = "\n---\n\n"

func (s *Sender) Send(message string) error {
	// Hard breaks (bot.MessageBreak) start a new message; each part is then
	// length-split as needed.
	for _, part := range strings.Split(message, bot.MessageBreak) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, chunk := range splitMessage(part) {
			if err := s.sendChunk(chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitMessage(message string) []string {
	if len(message) <= telegramMaxLen {
		return []string{message}
	}

	// Try splitting on repo separator (GitHub trending format)
	if strings.Contains(message, repoSeparator) {
		parts := strings.Split(message, repoSeparator)
		var chunks []string
		current := ""
		for i, part := range parts {
			segment := part
			if i < len(parts)-1 {
				segment += repoSeparator
			}
			if len(current)+len(segment) > telegramMaxLen {
				if current != "" {
					chunks = append(chunks, current)
				}
				current = segment
			} else {
				current += segment
			}
		}
		if current != "" {
			chunks = append(chunks, current)
		}
		return enforceLimit(chunks)
	}

	// Fallback: split on newlines
	lines := strings.Split(message, "\n")
	var chunks []string
	current := ""
	for _, line := range lines {
		// Handle single lines longer than the limit by hard-splitting them
		if len(line) > telegramMaxLen {
			if current != "" {
				chunks = append(chunks, strings.TrimRight(current, "\n"))
				current = ""
			}
			chunks = append(chunks, hardSplit(line, telegramMaxLen)...)
			continue
		}
		segment := line + "\n"
		if len(current)+len(segment) > telegramMaxLen {
			if current != "" {
				chunks = append(chunks, strings.TrimRight(current, "\n"))
			}
			current = segment
		} else {
			current += segment
		}
	}
	if current != "" {
		chunks = append(chunks, strings.TrimRight(current, "\n"))
	}
	return enforceLimit(chunks)
}

// enforceLimit splits any chunk that still exceeds the limit. A single segment
// can be larger than a whole message — one annotated repo entry runs to several
// KB — so neither splitting branch can be trusted to respect the limit itself.
func enforceLimit(chunks []string) []string {
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, hardSplit(chunk, telegramMaxLen)...)
	}
	return out
}

// hardSplit cuts s into pieces of at most max bytes, preferring a newline and
// falling back to a rune boundary. Telegram counts UTF-16 code units, which is
// never more than the byte count, so a byte limit is the conservative one.
func hardSplit(s string, max int) []string {
	var out []string
	for len(s) > max {
		cut := strings.LastIndexByte(s[:max], '\n')
		if cut <= 0 {
			cut = max
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
		}
		if cut <= 0 {
			cut = max
		}
		out = append(out, strings.TrimRight(s[:cut], "\n"))
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func (s *Sender) sendChunk(message string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", s.Token)
	payload, err := json.Marshal(map[string]any{
		"chat_id":    s.ChatID,
		"text":       message,
		"parse_mode": "HTML",
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := senderHTTPClient.Post(apiURL, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		// Scrub the token: net/http wraps the request URL (token in path) into the error. See secret-audit #47.
		return fmt.Errorf("failed to send message: %s", strings.ReplaceAll(err.Error(), s.Token, "<redacted>"))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Ok          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	if !result.Ok {
		return fmt.Errorf("telegram API error: %s", result.Description)
	}
	return nil
}

// GetChatID fetches the chat ID from the bot's recent updates.
// Requires the user to have sent /start to the bot first.
func GetChatID(token string) (int64, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", token)
	resp, err := senderHTTPClient.Get(url)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch updates: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response: %w", err)
	}

	var result struct {
		Ok     bool `json:"ok"`
		Result []struct {
			Message struct {
				Chat struct {
					ID        int64  `json:"id"`
					FirstName string `json:"first_name"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("failed to parse JSON: %w", err)
	}
	if !result.Ok || len(result.Result) == 0 {
		return 0, fmt.Errorf("no messages found — did you send /start to the bot?")
	}

	chat := result.Result[0].Message.Chat
	fmt.Printf("[OK] Found chat: %s (ID: %d)\n", chat.FirstName, chat.ID)
	return chat.ID, nil
}
