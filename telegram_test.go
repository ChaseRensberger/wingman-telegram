package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect Telegram request")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/bottest-token/getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"username":"demo_bot"}}`)
		case "/bottest-token/getUpdates":
			if body["offset"] != float64(7) || body["timeout"] != float64(30) || fmt.Sprint(body["allowed_updates"]) != "[message]" {
				t.Errorf("incorrect polling request: %+v", body)
			}
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":7,"message":{"message_id":2,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"Hello"}}]}`)
		case "/bottest-token/sendMessage":
			if body["chat_id"] != float64(42) || body["text"] != "a < b" || body["parse_mode"] != nil {
				t.Errorf("incorrect plain-text reply: %+v", body)
			}
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":3}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tg := newTelegram("test-token")
	tg.endpoint = server.URL
	ctx := context.Background()
	user, err := tg.me(ctx)
	if err != nil || user.ID != 99 {
		t.Fatalf("getMe: %+v, %v", user, err)
	}
	updates, err := tg.updates(ctx, 7, 30)
	if err != nil || len(updates) != 1 || updates[0].Message.Text != "Hello" {
		t.Fatalf("getUpdates: %+v, %v", updates, err)
	}
	if err := tg.send(ctx, 42, "a < b"); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramErrorsHideToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"bad secret-token"}`)
	}))
	tg := newTelegram("secret-token")
	tg.endpoint = server.URL
	if _, err := tg.me(context.Background()); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("API error exposed token: %v", err)
	}
	server.Close()
	if _, err := tg.me(context.Background()); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("transport error exposed token: %v", err)
	}
}
