package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TelegramConfig configures alert delivery via a Telegram bot.
type TelegramConfig struct {
	BotToken string   `yaml:"bot_token"`
	ChatIDs  []string `yaml:"chat_ids"`
	APIBase  string   `yaml:"api_base"`
	Proxy    string   `yaml:"proxy"`
}

// Notifier delivers alert messages (HTML formatted) asynchronously.
type Notifier interface {
	Notify(text string)
}

// Telegram sends messages through the Bot API with retries.
type Telegram struct {
	token  string
	chats  []string
	base   string
	client *http.Client
	queue  chan string
}

// NewTelegram returns nil (and no error) when no token is configured.
func NewTelegram(c TelegramConfig) (*Telegram, error) {
	if c.BotToken == "" {
		return nil, nil
	}
	if len(c.ChatIDs) == 0 {
		return nil, errors.New("telegram.chat_ids is empty; run `serverstatus-server telegram chats` to find yours")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.Proxy != "" {
		pu, err := url.Parse(c.Proxy)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("telegram.proxy %q is invalid", c.Proxy)
		}
		tr.Proxy = http.ProxyURL(pu) // http, https and socks5 schemes are supported
	}
	return &Telegram{
		token:  c.BotToken,
		chats:  c.ChatIDs,
		base:   strings.TrimRight(c.APIBase, "/"),
		client: &http.Client{Transport: tr, Timeout: 15 * time.Second},
		queue:  make(chan string, 256),
	}, nil
}

// Notify queues a message for all chats without blocking.
func (t *Telegram) Notify(text string) {
	select {
	case t.queue <- text:
	default:
		log.Printf("telegram: queue full, dropping message")
	}
}

// Run delivers queued messages until ctx is cancelled, then drains briefly.
func (t *Telegram) Run(ctx context.Context) {
	for {
		select {
		case msg := <-t.queue:
			t.deliver(ctx, msg)
		case <-ctx.Done():
			drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				select {
				case msg := <-t.queue:
					t.deliver(drain, msg)
				default:
					return
				}
			}
		}
	}
}

func (t *Telegram) deliver(ctx context.Context, msg string) {
	for _, chat := range t.chats {
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			var wait time.Duration
			wait, err = t.send(ctx, chat, msg)
			if err == nil {
				break
			}
			if wait == 0 {
				wait = time.Duration(1<<attempt) * 2 * time.Second
			}
			select {
			case <-ctx.Done():
				log.Printf("telegram: giving up on chat %s: %v", chat, err)
				return
			case <-time.After(wait):
			}
		}
		if err != nil {
			log.Printf("telegram: send to chat %s failed: %v", chat, err)
		}
	}
}

// Send delivers one message synchronously (used by the CLI test command).
func (t *Telegram) Send(ctx context.Context, chat, text string) error {
	_, err := t.send(ctx, chat, text)
	return err
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call invokes a Bot API method. The token is part of the URL, so every
// error is scrubbed before it can reach a log.
func (t *Telegram) call(ctx context.Context, method string, payload any) (*tgResponse, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/bot"+t.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, t.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.redact(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r tgResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("HTTP %d: unexpected response", resp.StatusCode)
	}
	if !r.OK {
		return &r, fmt.Errorf("HTTP %d: %s", resp.StatusCode, r.Description)
	}
	return &r, nil
}

// send returns a suggested wait before retrying (Telegram rate limits).
func (t *Telegram) send(ctx context.Context, chat, text string) (time.Duration, error) {
	r, err := t.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  chat,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	if err != nil && r != nil && r.Parameters.RetryAfter > 0 {
		return time.Duration(r.Parameters.RetryAfter) * time.Second, err
	}
	return 0, err
}

func (t *Telegram) redact(err error) error {
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<token>"))
}

// TelegramChat is a chat seen in recent bot updates.
type TelegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	User  string `json:"username"`
	First string `json:"first_name"`
}

// RecentChats lists chats that recently messaged the bot (getUpdates).
func (t *Telegram) RecentChats(ctx context.Context) ([]TelegramChat, error) {
	r, err := t.call(ctx, "getUpdates", map[string]any{"limit": 100})
	if err != nil {
		return nil, err
	}
	var updates []map[string]json.RawMessage
	if err := json.Unmarshal(r.Result, &updates); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []TelegramChat
	for _, u := range updates {
		for _, k := range []string{"message", "channel_post", "my_chat_member", "edited_message"} {
			var m struct {
				Chat TelegramChat `json:"chat"`
			}
			if raw, ok := u[k]; ok && json.Unmarshal(raw, &m) == nil && m.Chat.ID != 0 && !seen[m.Chat.ID] {
				seen[m.Chat.ID] = true
				out = append(out, m.Chat)
			}
		}
	}
	return out, nil
}

// TelegramForSetup builds a client for the CLI even when chat_ids is empty.
func TelegramForSetup(c TelegramConfig) (*Telegram, error) {
	if c.BotToken == "" {
		return nil, errors.New("telegram.bot_token is not set in the config")
	}
	if len(c.ChatIDs) == 0 {
		c.ChatIDs = []string{"0"}
	}
	return NewTelegram(c)
}

// Chats returns the configured chat IDs.
func (t *Telegram) Chats() []string { return t.chats }
