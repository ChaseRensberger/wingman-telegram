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
	"sync"
	"testing"
	"time"
)

func TestPollingQueuesUpdatesDuringRunAndResumesInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	b, tg := testBot()
	b.state, b.save = current, file.save
	if err := b.handle(context.Background(), privateUpdate(1, "First task")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	w := b.wingman.(*fakeWingman)
	w.resultFn = func(ctx context.Context, _ string, p *pendingReply, save func() error) (string, error) {
		p.Events.Seq = 7
		p.Events.Messages = []replyMessage{{ID: "reply", Revision: 1, Text: "Saved text"}}
		if err := save(); err != nil {
			return "", err
		}
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	var offsets []int64
	tg.poll = func(ctx context.Context, offset int64, _ int) ([]update, error) {
		offsets = append(offsets, offset)
		if len(offsets) == 1 {
			select {
			case <-started:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			unauthorized := privateUpdate(4, "Ignore this")
			unauthorized.Message.From.ID = 99
			return []update{privateUpdate(2, "Second task"), privateUpdate(2, "Second task"), privateUpdate(3, "/session"), unauthorized}, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var durable state
		if err := json.Unmarshal(data, &durable); err != nil {
			return nil, err
		}
		if durable.Offset != 5 || durable.Pending == nil || durable.Pending.RunID != "run-1" || len(durable.Queue) != 2 || durable.EventSeq != 7 || len(durable.Pending.Events.Messages) != 1 {
			t.Errorf("updates acknowledged without durable queue: %+v", durable)
		}
		cancel()
		return nil, ctx.Err()
	}
	if err := b.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected run error: %v", err)
	}
	file.close()
	if !reflect.DeepEqual(offsets, []int64{2, 5}) || len(w.requests) != 1 {
		t.Fatalf("polling stopped or tasks ran concurrently: offsets=%v requests=%v", offsets, w.requests)
	}

	file, restored, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	restarted, newTG := testBot()
	restarted.state, restarted.wingman = restored, w
	var resumedCursors []int64
	w.resultFn = func(_ context.Context, _ string, p *pendingReply, _ func() error) (string, error) {
		resumedCursors = append(resumedCursors, p.Events.Seq)
		return "Done", nil
	}
	ctx, cancelRestart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRestart()
	restarted.save = func(s state) error {
		if err := file.save(s); err != nil {
			return err
		}
		if s.Pending == nil && len(s.Queue) == 0 {
			cancelRestart()
		}
		return nil
	}
	newTG.poll = func(ctx context.Context, offset int64, _ int) ([]update, error) {
		if offset != 5 {
			t.Errorf("restart forgot queued progress: %d", offset)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := restarted.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected restart error: %v", err)
	}
	if w.created != 1 || !reflect.DeepEqual(w.requests, []string{"telegram:0:42:1", "telegram:0:42:2"}) {
		t.Fatalf("restart repeated or lost tasks: %+v", w)
	}
	if len(newTG.sent) != 4 || newTG.sent[0] != "Done" || !strings.HasPrefix(newTG.sent[1], "Wingman accepted") || newTG.sent[2] != "Done" || newTG.sent[3] != "https://console.example/console/sessions/session-1" {
		t.Fatalf("queue was not processed in order: %v", newTG.sent)
	}
	if restarted.state.Pending != nil || len(restarted.state.Queue) != 0 || restarted.state.Offset != 5 || !reflect.DeepEqual(resumedCursors, []int64{7, 7}) {
		t.Fatalf("task checkpoints overwrote queued progress: %+v", restarted.state)
	}
}

func TestQueueSaveFailureStopsBeforeAcknowledgingUpdates(t *testing.T) {
	b, tg := testBot()
	if err := b.handle(context.Background(), privateUpdate(1, "First task")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	w := b.wingman.(*fakeWingman)
	w.resultFn = func(ctx context.Context, _ string, _ *pendingReply, _ func() error) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	want := errors.New("disk full")
	b.save = func(s state) error {
		if len(s.Queue) != 0 {
			return want
		}
		return nil
	}
	polls := 0
	tg.poll = func(ctx context.Context, offset int64, _ int) ([]update, error) {
		polls++
		if offset != 2 || polls != 1 {
			t.Errorf("acknowledged an update without saving it: offset=%d polls=%d", offset, polls)
		}
		select {
		case <-started:
			return []update{privateUpdate(2, "Second task")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := b.run(ctx); !errors.Is(err, want) {
		t.Fatalf("expected storage failure, got %v", err)
	}
	if polls != 1 || len(w.requests) != 1 {
		t.Fatalf("continued after storage failure: polls=%d requests=%v", polls, w.requests)
	}
}

func TestPollingPersistsUpdatesWhileWingmanIsUnavailable(t *testing.T) {
	checking := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			t.Errorf("submitted work before readiness: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"ready":false}`)
		once.Do(func() { close(checking) })
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	b, tg := testBot()
	b.state, b.save, b.wingman = current, file.save, testWingman(t, server)
	if err := b.handle(context.Background(), privateUpdate(1, "First task")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	polls := 0
	tg.poll = func(ctx context.Context, offset int64, _ int) ([]update, error) {
		polls++
		if polls == 1 {
			select {
			case <-checking:
				return []update{privateUpdate(2, "Next task"), privateUpdate(3, "/help")}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
		} else {
			var saved state
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Error(err)
			} else if offset != 4 || saved.Offset != 4 || len(saved.Queue) != 2 || saved.Pending == nil || saved.Pending.RunID != "" || saved.Pending.Text != "First task" {
				t.Errorf("polling acknowledged unsaved work during outage: %+v", saved)
			}
		}
		cancel()
		return nil, ctx.Err()
	}
	if err := b.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("polling failed during readiness wait: %v", err)
	}
	if polls != 2 || len(tg.sent) != 0 {
		t.Fatalf("unexpected outage progress: polls=%d replies=%v", polls, tg.sent)
	}
}

func TestRestartDeliversSavedRepliesWithoutWingman(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("saved delivery contacted Wingman: %s", r.URL.Path)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	file, saved, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	saved.Offset, saved.LastUpdateAt = 3, time.Now().Unix()
	saved.Pending = &pendingReply{ChatID: 42, Text: "Task", RequestID: "saved-request", RunID: "run-1", Replies: []string{"Already delivered", "\n", "Remaining text", " \t"}, Sent: 1}
	saved.Queue = []update{privateUpdate(2, "/help")}
	if err := file.save(saved); err != nil {
		t.Fatal(err)
	}
	file.close()
	file, saved, err = openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	b, tg := testBot()
	b.state, b.wingman = saved, testWingman(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.save = func(s state) error {
		if err := file.save(s); err != nil {
			return err
		}
		if s.Pending == nil && len(s.Queue) == 0 {
			cancel()
		}
		return nil
	}
	tg.poll = func(ctx context.Context, _ int64, _ int) ([]update, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := b.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("saved delivery failed: %v", err)
	}
	if len(tg.sent) != 2 || tg.sent[0] != "Remaining text" || !strings.Contains(tg.sent[1], "Build agent") || b.state.Pending != nil || len(b.state.Queue) != 0 {
		t.Fatalf("blank chunks or outage blocked saved delivery: replies=%q state=%+v", tg.sent, b.state)
	}
}
