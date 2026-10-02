package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEventCheckpointResumesWithoutLosingOutput(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		cursors = append(cursors, r.URL.Query().Get("after"))
		if len(cursors) == 1 {
			sendEvent(w, 1, "session.message.created", messageEvent("other", "old", "Other task", 1))
			sendEvent(w, 2, "session.message.created", messageEvent("run-1", "reply", "Saved final text", 3))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		sendEvent(w, 2, "session.message.created", messageEvent("run-1", "reply", "Duplicate event", 3))
		sendEvent(w, 0, "session.message.created", messageEvent("run-1", "volatile", "Unsaved live text", 4))
		sendEvent(w, 3, "future.event", map[string]string{"extra": "ignored"})
		sendEvent(w, 4, "session.message.created", messageEvent("run-1", "reply", "Older revision", 2))
		sendEvent(w, 5, "session.run.completed", map[string]string{"run_id": "run-1"})
	}))
	defer server.Close()
	w := testWingman(t, server)
	p := &pendingReply{RunID: "run-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var durable []byte
	save := func() error {
		var err error
		durable, err = json.Marshal(p)
		if p.Events.Seq == 2 {
			cancel()
		}
		return err
	}
	if _, err := w.result(ctx, "session-1", p, save); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interrupted stream, got %v", err)
	}
	p = &pendingReply{}
	if err := json.Unmarshal(durable, p); err != nil {
		t.Fatal(err)
	}
	if p.Events.Seq != 2 || len(p.Events.Messages) != 1 || p.Events.Messages[0].Text != "Saved final text" {
		t.Fatalf("cursor and output were not saved together: %+v", p.Events)
	}
	ctx, cancelRestart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRestart()
	text, err := w.result(ctx, "session-1", p, func() error {
		var err error
		durable, err = json.Marshal(p)
		return err
	})
	if err != nil || text != "Saved final text" || p.Events.Seq != 5 || !reflect.DeepEqual(cursors, []string{"0", "2"}) {
		t.Fatalf("incorrect event recovery: text=%q events=%+v cursors=%v err=%v", text, p.Events, cursors, err)
	}
	// A crash after the terminal checkpoint but before preparing Telegram
	// replies must finish from saved output without opening another stream.
	p = &pendingReply{}
	if err := json.Unmarshal(durable, p); err != nil {
		t.Fatal(err)
	}
	text, err = w.result(ctx, "session-1", p, func() error { return nil })
	if err != nil || text != "Saved final text" || len(cursors) != 2 {
		t.Fatalf("terminal checkpoint was not resumable: %q %v", text, err)
	}
}

func TestStreamReconnectReloadsRunAndPreservesCursor(t *testing.T) {
	for _, resync := range []bool{false, true} {
		t.Run(fmt.Sprint(resync), func(t *testing.T) {
			var paths, cursors []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/sessions/session-1/events":
					w.Header().Set("Content-Type", "text/event-stream")
					cursors = append(cursors, r.URL.Query().Get("after"))
					if len(cursors) == 1 {
						sendEvent(w, 7, "session.message.created", messageEvent("run-1", "reply", "Initial text", 1))
						if resync {
							sendEvent(w, 999, "session.events.resync_required", map[string]any{"cursor": 999, "reason": "overflow"})
						}
						return
					}
					sendEvent(w, 8, "session.message.created", messageEvent("run-1", "reply", "Final text", 2))
					sendEvent(w, 9, "session.run.completed", map[string]string{"run_id": "run-1"})
				case "/sessions/session-1/runs/run-1":
					fmt.Fprint(w, `{"id":"run-1","status":"running"}`)
				case "/sessions/session-1":
					fmt.Fprint(w, `{"id":"session-1","history":[]}`)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			w := testWingman(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			p := &pendingReply{RunID: "run-1", Events: runEvents{Seq: 4}}
			text, err := w.result(ctx, "session-1", p, func() error { return nil })
			wantPaths := []string{"/sessions/session-1/events", "/sessions/session-1/runs/run-1", "/sessions/session-1", "/sessions/session-1/events"}
			if err != nil || text != "Final text" || !reflect.DeepEqual(cursors, []string{"4", "7"}) || !reflect.DeepEqual(paths, wantPaths) {
				t.Fatalf("incorrect reconnect: text=%q cursors=%v paths=%v err=%v", text, cursors, paths, err)
			}
		})
	}
}

func TestTerminalSnapshotRecoveryIsRunSpecific(t *testing.T) {
	for _, mode := range []string{"completed", "synchronized", "unavailable stream", "failed", "aborted"} {
		t.Run(mode, func(t *testing.T) {
			status := mode
			if mode == "synchronized" || mode == "unavailable stream" {
				status = "completed"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/sessions/session-1/events":
					if mode == "unavailable stream" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":{"code":"unavailable","message":"try later"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if mode == "synchronized" {
						sendEvent(w, 900, "session.events.synchronized", map[string]int{"cursor": 900, "watermark": 900})
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}
				case "/sessions/session-1/runs/run-1":
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "run-1", "status": status, "error_message": "Task stopped"})
				case "/sessions/session-1":
					fmt.Fprint(w, `{"id":"session-1","history":[
						{"id":"ours","role":"assistant","state":"completed","revision":3,"content":[{"type":"text","text":"Our final reply"},{"type":"reasoning","reasoning":"secret reasoning"}]},
						{"id":"theirs","role":"assistant","state":"completed","content":[{"type":"text","text":"Other run reply"}]}
					]}`)
				case "/sessions/session-1/model-calls":
					fmt.Fprint(w, `[{"run_id":"run-1","assistant_message_id":"ours"},{"run_id":"other-run","assistant_message_id":"theirs"}]`)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			w := testWingman(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			p := &pendingReply{RunID: "run-1", Events: runEvents{Seq: 10}}
			saved := false
			text, err := w.result(ctx, "session-1", p, func() error { saved = true; return nil })
			if err != nil || !saved || p.Events.Seq != 10 || p.Events.Status != status {
				t.Fatalf("failed snapshot recovery: events=%+v saved=%v err=%v", p.Events, saved, err)
			}
			if status == "completed" && text != "Our final reply" || status != "completed" && !strings.Contains(text, status) {
				t.Fatalf("incorrect run output: %q", text)
			}
		})
	}
}

func TestEventSaveFailureDoesNotSkipOutputOnRestart(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursors = append(cursors, r.URL.Query().Get("after"))
		w.Header().Set("Content-Type", "text/event-stream")
		sendEvent(w, 1, "session.message.created", messageEvent("run-1", "reply", "Final text", 1))
		sendEvent(w, 2, "session.run.completed", map[string]string{"run_id": "run-1"})
	}))
	defer server.Close()
	w := testWingman(t, server)
	p := &pendingReply{RunID: "run-1"}
	want := errors.New("disk full")
	if _, err := w.result(context.Background(), "session-1", p, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("storage failure was swallowed: %v", err)
	}
	// Reload the last durable state, before either cursor or output was saved.
	p = &pendingReply{RunID: "run-1"}
	text, err := w.result(context.Background(), "session-1", p, func() error { return nil })
	if err != nil || text != "Final text" || !reflect.DeepEqual(cursors, []string{"0", "0"}) {
		t.Fatalf("unsaved event was skipped: %q %v %v", text, cursors, err)
	}
}
