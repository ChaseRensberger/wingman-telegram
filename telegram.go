package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type telegramUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type telegramMessage struct {
	ID   int64         `json:"message_id"`
	From *telegramUser `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text string `json:"text"`
}

type update struct {
	ID      int64            `json:"update_id"`
	Message *telegramMessage `json:"message"`
}

type telegram struct {
	endpoint string
	token    string
	http     *http.Client
}

func newTelegram(token string) *telegram {
	return &telegram{endpoint: "https://api.telegram.org", token: token, http: &http.Client{Timeout: 40 * time.Second}}
}

func (t *telegram) call(ctx context.Context, method string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+"/bot"+t.token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := t.http.Do(req)
	if err != nil {
		// HTTP errors include the request URL, which contains the bot token.
		return fmt.Errorf("Telegram %s request failed: %s", method, strings.ReplaceAll(err.Error(), t.token, "[redacted]"))
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("Telegram %s returned an invalid response (HTTP %d)", method, response.StatusCode)
	}
	if !envelope.OK {
		return fmt.Errorf("Telegram %s failed (%d): %s", method, envelope.ErrorCode, strings.ReplaceAll(envelope.Description, t.token, "[redacted]"))
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

func (t *telegram) me(ctx context.Context) (telegramUser, error) {
	var user telegramUser
	err := t.call(ctx, "getMe", struct{}{}, &user)
	return user, err
}

func (t *telegram) updates(ctx context.Context, offset int64, timeout int) ([]update, error) {
	var updates []update
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"message"},
	}, &updates)
	return updates, err
}

func (t *telegram) send(ctx context.Context, chatID int64, text string) error {
	return t.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true},
	}, nil)
}
