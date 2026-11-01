// Package notify delivers trade notifications. The only implementation is a
// Telegram bot: messages are pushed to a configured chat via the Bot API, and
// incoming messages from that chat can be received via long polling.
package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Telegram sends and receives messages through the Telegram Bot API.
type Telegram struct {
	botToken string
	chatID   string
	client   *http.Client // short requests (sendMessage)
	pollCli  *http.Client // long polling (getUpdates); timeout must exceed the server wait

	pollStop   chan struct{}
	pollStopMu sync.Mutex
}

// NewTelegram creates a Telegram notifier. Both token and chat ID must be
// non-empty for messages to be sent or received.
func NewTelegram(botToken, chatID string) *Telegram {
	return &Telegram{
		botToken: strings.TrimSpace(botToken),
		chatID:   strings.TrimSpace(chatID),
		client:   &http.Client{Timeout: 10 * time.Second},
		pollCli:  &http.Client{Timeout: 45 * time.Second},
	}
}

// Enabled reports whether the notifier has the required configuration.
func (t *Telegram) Enabled() bool {
	return t != nil && t.botToken != "" && t.chatID != ""
}

// AllowsChat reports whether messages from the given chat should be handled.
// Only the configured chat is accepted.
func (t *Telegram) AllowsChat(chatID int64) bool {
	if t == nil || t.chatID == "" {
		return false
	}
	return t.chatID == strconv.FormatInt(chatID, 10)
}

// Send pushes a Markdown-formatted message to the configured chat.
func (t *Telegram) Send(message string) error {
	return t.send(message, "Markdown")
}

// SendPlain pushes a plain-text message (no Markdown parsing) to the
// configured chat — used for free-form AI replies that would break Markdown.
func (t *Telegram) SendPlain(message string) error {
	return t.send(message, "")
}

func (t *Telegram) send(message, parseMode string) error {
	if !t.Enabled() {
		return nil
	}

	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	form := url.Values{}
	form.Set("chat_id", t.chatID)
	form.Set("text", message)
	if parseMode != "" {
		form.Set("parse_mode", parseMode)
	}

	resp, err := t.client.PostForm(endpoint, form)
	if err != nil {
		return fmt.Errorf("telegram send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("telegram send: unexpected status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// telegramUpdate is the subset of a Bot API update we care about.
type telegramUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

// PollMessages long-polls incoming messages and calls handle for each text
// message. It blocks until StopPolling is called; transient errors are logged
// and retried after a short backoff. Run it in its own goroutine.
func (t *Telegram) PollMessages(handle func(chatID int64, from, text string)) {
	if !t.Enabled() {
		return
	}

	var offset int64
	for {
		select {
		case <-t.stopChan():
			return
		default:
		}

		updates, err := t.getUpdates(offset, 15)
		if err != nil {
			// A stopped long poll surfaces as a client error; exit quietly.
			select {
			case <-t.stopChan():
				return
			default:
			}
			log.Printf("⚠️ telegram getUpdates failed: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			from := ""
			if u.Message.From != nil {
				if u.Message.From.Username != "" {
					from = "@" + u.Message.From.Username
				} else {
					from = u.Message.From.FirstName
				}
			}
			handle(u.Message.Chat.ID, from, u.Message.Text)
		}
	}
}

// StopPolling stops a running PollMessages loop. Safe to call more than once.
func (t *Telegram) StopPolling() {
	t.pollStopMu.Lock()
	defer t.pollStopMu.Unlock()
	if t.pollStop == nil {
		t.pollStop = make(chan struct{})
		return
	}
	select {
	case <-t.pollStop:
		// already closed
	default:
		close(t.pollStop)
	}
}

func (t *Telegram) stopChan() <-chan struct{} {
	t.pollStopMu.Lock()
	defer t.pollStopMu.Unlock()
	if t.pollStop == nil {
		t.pollStop = make(chan struct{})
	}
	return t.pollStop
}

// getUpdates performs one long-poll request against the Bot API. GET is used
// (instead of POST) because some proxies terminate long-idle POST requests.
func (t *Telegram) getUpdates(offset int64, timeoutSec int) ([]telegramUpdate, error) {
	params := url.Values{}
	if offset > 0 {
		params.Set("offset", strconv.FormatInt(offset, 10))
	}
	params.Set("timeout", strconv.Itoa(timeoutSec))
	params.Set("allowed_updates", `["message"]`)
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?%s", t.botToken, params.Encode())

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates: %w", err)
	}

	resp, err := t.pollCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram getUpdates: unexpected status %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	var envelope struct {
		OK     bool             `json:"ok"`
		Result []telegramUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("telegram getUpdates decode: %w", err)
	}
	return envelope.Result, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
