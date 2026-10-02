package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeTelegram struct {
	sent []string
	err  error
	poll func(context.Context, int64, int) ([]update, error)
}

func (f *fakeTelegram) updates(ctx context.Context, offset int64, timeout int) ([]update, error) {
	if f.poll != nil {
		return f.poll(ctx, offset, timeout)
	}
	return nil, nil
}

func (f *fakeTelegram) send(_ context.Context, _ int64, text string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, text)
	return nil
}

func privateUpdate(id int64, text string) update {
	m := &telegramMessage{ID: id, From: &telegramUser{ID: 42}, Text: text}
	m.Chat.ID, m.Chat.Type = 42, "private"
	return update{ID: id, Message: m}
}

func testBot() (*bot, *fakeTelegram) {
	tg := &fakeTelegram{}
	b := &bot{telegram: tg, userID: 42, wingman: &fakeWingman{}, consoleURL: "https://console.example", save: func(state) error { return nil }}
	return b, tg
}

func TestTaskAndDuplicateDelivery(t *testing.T) {
	b, tg := testBot()
	ctx := context.Background()
	u := privateUpdate(1, "Change the app")
	if err := b.handle(ctx, u); err != nil {
		t.Fatal(err)
	}
	if b.state.Pending == nil || len(tg.sent) != 0 {
		t.Fatal("reply must be persisted before delivery")
	}
	if b.state.Pending.Text != "Change the app" || b.state.Pending.RequestID == "" {
		t.Fatal("task does not match the requested text")
	}
	if err := b.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.handle(ctx, u); err != nil {
		t.Fatal(err)
	}
	if b.state.Pending != nil || len(tg.sent) != 2 || tg.sent[1] != "Done" {
		t.Fatal("duplicate delivery repeated the reply")
	}
}

func TestUnauthorizedUpdatesReceiveNoReply(t *testing.T) {
	for _, kind := range []string{"other user", "group", "missing sender", "different chat", "missing message"} {
		t.Run(kind, func(t *testing.T) {
			b, tg := testBot()
			u := privateUpdate(1, "Hello")
			switch kind {
			case "other user":
				u.Message.From.ID = 100
			case "group":
				u.Message.Chat.Type = "group"
			case "missing sender":
				u.Message.From = nil
			case "different chat":
				u.Message.Chat.ID = 100
			case "missing message":
				u.Message = nil
			}
			if err := b.handle(context.Background(), u); err != nil {
				t.Fatal(err)
			}
			if b.state.Pending != nil || b.state.Offset != 2 || len(tg.sent) != 0 {
				t.Fatal("unauthorized update was processed")
			}
		})
	}
}

func TestCommandsAndAttachments(t *testing.T) {
	b, tg := testBot()
	ctx := context.Background()
	for i, text := range []string{"/start", "/help", "/session", "/unknown", ""} {
		if err := b.handle(ctx, privateUpdate(int64(i+1), text)); err != nil {
			t.Fatal(err)
		}
		if err := b.finish(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(tg.sent) != 5 || !strings.Contains(tg.sent[0], "Assist agent") || tg.sent[2] != "Send a task first to create a session." {
		t.Fatalf("incorrect command responses: %+v", tg.sent)
	}
	if tg.sent[4] != "Send a text message. Attachments are not supported." {
		t.Fatal("attachment was accepted")
	}
}

func TestRestartResumesPendingReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	b, tg := testBot()
	b.state, b.save = current, file.save
	ctx := context.Background()
	if err := b.handle(ctx, privateUpdate(1, "Hello")); err != nil {
		t.Fatal(err)
	}
	tg.err = errors.New("disconnected")
	if err := b.finish(ctx); err == nil {
		t.Fatal("expected delivery error")
	}
	file.close()
	file, restored, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	restarted, newTelegram := testBot()
	restarted.state, restarted.save = restored, file.save
	if err := restarted.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.handle(ctx, privateUpdate(1, "Hello")); err != nil {
		t.Fatal(err)
	}
	if restarted.state.Pending != nil || len(newTelegram.sent) != 2 || newTelegram.sent[1] != "Done" {
		t.Fatal("pending reply was not restored")
	}
}

func TestStateFailureStopsBeforeReply(t *testing.T) {
	b, tg := testBot()
	b.save = func(state) error { return errors.New("disk full") }
	if err := b.handle(context.Background(), privateUpdate(1, "Hello")); err == nil {
		t.Fatal("expected save error")
	}
	if len(tg.sent) != 0 {
		t.Fatal("reply was sent without durable state")
	}
}

func TestStatePersistsLocksAndRestrictsAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	current.Offset = 7
	current.LastUpdateAt = time.Now().Unix()
	current.Pending = &pendingReply{ChatID: 42, Text: "Request recieved."}
	if err := file.save(current); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openState(path, "scope"); err == nil {
		t.Fatal("second process acquired state")
	}
	file.close()
	file, loaded, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	file.close()
	if !reflect.DeepEqual(loaded, current) {
		t.Fatalf("state was not restored: %+v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
	if _, _, err := openState(path, "different"); err == nil {
		t.Fatal("state accepted a different bot or user")
	}
}

func TestPollingOverHTTPPersistsProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var offsets []int64
	var sent []string
	delivered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/bottoken/getUpdates":
			offsets = append(offsets, int64(body["offset"].(float64)))
			if len(offsets) == 1 {
				fmt.Fprint(w, `{"ok":true,"result":[{"update_id":5,"message":{"message_id":1,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"Hello"}}]}`)
			} else {
				select {
				case <-delivered:
				case <-r.Context().Done():
					return
				}
				cancel()
				fmt.Fprint(w, `{"ok":true,"result":[]}`)
			}
		case "/bottoken/sendMessage":
			sent = append(sent, body["text"].(string))
			fmt.Fprint(w, `{"ok":true,"result":{}}`)
		default:
			t.Errorf("unexpected outgoing request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tg := newTelegram("token")
	tg.endpoint = server.URL
	b, _ := testBot()
	b.telegram = tg
	var durable state
	b.save = func(s state) error {
		data, _ := json.Marshal(s)
		durable = state{}
		if err := json.Unmarshal(data, &durable); err != nil {
			return err
		}
		if durable.Offset == 6 && durable.Pending == nil {
			close(delivered)
		}
		return nil
	}
	_ = b.run(ctx)
	if !reflect.DeepEqual(offsets, []int64{0, 6}) || len(sent) != 2 || sent[1] != "Done" || durable.Offset != 6 || durable.Pending != nil {
		t.Fatalf("polling did not persist progress: %+v %+v %+v", offsets, sent, durable)
	}
}

func TestMissingUserIDDoesNotRequireWingman(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("TELEGRAM_USER_ID", "")
	t.Setenv("WINGMAN_PASSWORD", "")
	err := run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_USER_ID") {
		t.Fatalf("unexpected startup requirement: %v", err)
	}
}

func TestStaleOffsetResetsOnlyAfterRetention(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, age := range []time.Duration{23 * time.Hour, 24 * time.Hour, 8 * 24 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			b, _ := testBot()
			b.state = state{Offset: 100, LastUpdateAt: now.Add(-age).Unix()}
			saves := 0
			b.save = func(s state) error {
				saves++
				if s.Offset != 0 || s.LastUpdateAt != now.Unix() {
					t.Fatalf("incorrect reset state: %+v", s)
				}
				return nil
			}
			if err := b.resetStaleOffset(now); err != nil {
				t.Fatal(err)
			}
			if age < 24*time.Hour {
				if saves != 0 || b.state.Offset != 100 {
					t.Fatal("recent progress was reset")
				}
			} else if saves != 1 || b.state.Offset != 0 {
				t.Fatal("stale progress was not durably reset")
			}
		})
	}
}

func TestRestartAfterInactivityAcceptsLowerUpdateID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	current.Offset = 100
	current.LastUpdateAt = time.Now().Add(-8 * 24 * time.Hour).Unix()
	current.Pending = &pendingReply{ChatID: 42, Text: "old reply"}
	if err := file.save(current); err != nil {
		t.Fatal(err)
	}
	file.close()
	file, restored, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	b, tg := testBot()
	b.state, b.save = restored, file.save
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var offsets []int64
	delivered := make(chan struct{})
	b.save = func(s state) error {
		if err := file.save(s); err != nil {
			return err
		}
		if s.Offset == 6 && s.Pending == nil && len(s.Queue) == 0 {
			close(delivered)
		}
		return nil
	}
	tg.poll = func(_ context.Context, offset int64, _ int) ([]update, error) {
		offsets = append(offsets, offset)
		if len(offsets) == 1 {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved state
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Offset != 0 {
				t.Fatalf("reset not persisted before polling: %+v", saved)
			}
			return []update{privateUpdate(5, "Hello"), privateUpdate(5, "Hello")}, nil
		}
		select {
		case <-delivered:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		cancel()
		return nil, ctx.Err()
	}
	if err := b.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected run error: %v", err)
	}
	if !reflect.DeepEqual(offsets, []int64{0, 6}) || len(tg.sent) != 3 || tg.sent[0] != "old reply" || tg.sent[2] != "Done" {
		t.Fatalf("lower update was lost or duplicated: offsets=%v sent=%v", offsets, tg.sent)
	}
	if b.state.LastUpdateAt <= current.LastUpdateAt {
		t.Fatal("last update time was not refreshed")
	}
}

func TestStaleOffsetSaveFailureStopsBeforePolling(t *testing.T) {
	b, tg := testBot()
	b.state = state{Offset: 100, LastUpdateAt: time.Now().Add(-8 * 24 * time.Hour).Unix()}
	want := errors.New("disk full")
	b.save = func(state) error { return want }
	tg.poll = func(context.Context, int64, int) ([]update, error) {
		t.Fatal("polled without durable reset")
		return nil, nil
	}
	if err := b.run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("expected save failure, got %v", err)
	}
}

func TestLegacyStateUsesModificationTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"scope":"scope","offset":100}`), 0600); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	file, loaded, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	if loaded.Offset != 100 || loaded.LastUpdateAt != modified.Unix() {
		t.Fatalf("legacy timestamp was not migrated: %+v", loaded)
	}
}
