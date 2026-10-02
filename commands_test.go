package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQueuedCommandsSwitchSessionAndKeepOrderAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testBot()
	b.state, b.save = current, file.save
	w := b.wingman.(*fakeWingman)
	var sessions, commands []string
	var cursors []int64
	w.resultFn = func(_ context.Context, id string, p *pendingReply, save func() error) (string, error) {
		sessions = append(sessions, id)
		commands = append(commands, p.Command)
		cursors = append(cursors, p.Events.Seq)
		p.Events.Seq += 10
		p.Events.Status = "completed"
		return "Done", save()
	}
	for i, text := range []string{"First task", "/new", "Second task", "/compact", "Follow-up"} {
		if err := b.handle(context.Background(), privateUpdate(int64(i+1), text)); err != nil {
			t.Fatal(err)
		}
	}
	if w.created != 0 || len(b.state.Queue) != 4 {
		t.Fatal("commands executed ahead of the first task")
	}
	if err := b.finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	file.close()
	file, restored, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	restarted, tg := testBot()
	restarted.state, restarted.wingman = restored, w
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	restarted.save = func(s state) error {
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
	if err := restarted.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if w.created != 2 || !reflect.DeepEqual(sessions, []string{"session-1", "session-2", "session-2", "session-2"}) || !reflect.DeepEqual(commands, []string{"", "", "compact", ""}) {
		t.Fatalf("incorrect command order or session: sessions=%v commands=%v created=%d", sessions, commands, w.created)
	}
	if !reflect.DeepEqual(cursors, []int64{0, 0, 10, 20}) || restarted.state.EventSeq != 30 {
		t.Fatalf("incorrect event cursor after session switch: %v state=%+v", cursors, restarted.state)
	}
	if len(tg.sent) != 7 || !strings.Contains(tg.sent[0], "Started a new conversation.\nhttps://console.example/console/sessions/session-2") || !strings.HasPrefix(tg.sent[4], "Conversation compacted.") {
		t.Fatalf("incorrect replies: %v", tg.sent)
	}
}

func TestNewSessionReplyResumesWithoutCreatingAnotherSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	b, tg := testBot()
	w := b.wingman.(*fakeWingman)
	b.state, b.save = current, file.save
	b.state.SessionID, b.state.EventSeq = "old-session", 99
	u := privateUpdate(1, "/new")
	if err := b.handle(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	tg.err = errors.New("Telegram unavailable")
	if err := b.finish(context.Background()); !errors.Is(err, tg.err) {
		t.Fatal(err)
	}
	file.close()
	file, restored, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	if restored.SessionID != "session-1" || restored.EventSeq != 0 || restored.Pending.Command != "new" {
		t.Fatalf("session switch was not persisted: %+v", restored)
	}
	restarted, newTG := testBot()
	restarted.state, restarted.save, restarted.wingman = restored, file.save, w
	if err := restarted.finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.handle(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if w.created != 1 || len(w.requests) != 0 || len(newTG.sent) != 1 || restarted.state.Pending != nil {
		t.Fatal("restart or duplicate update repeated session creation")
	}
}

func TestCompactRequiresExistingSession(t *testing.T) {
	b, tg := testBot()
	if err := b.handle(context.Background(), privateUpdate(1, "/compact")); err != nil {
		t.Fatal(err)
	}
	if err := b.finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := b.wingman.(*fakeWingman)
	if w.created != 0 || len(w.requests) != 0 || len(tg.sent) != 1 || !strings.Contains(tg.sent[0], "no conversation to compact") {
		t.Fatalf("unexpected empty-session compaction: %v", tg.sent)
	}
}

func TestCompactHTTPAndRestart(t *testing.T) {
	for _, scenario := range []string{"completed", "failed", "aborted", "rejected", "restart accepted", "restart uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				username, password, ok := r.BasicAuth()
				if !ok || username != "wingman" || password != "secret" || r.Header.Get("X-Wingman-Client") != "" {
					t.Error("incorrect authentication or client identity")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /ready":
					fmt.Fprint(w, `{"ready":true}`)
				case "GET /agents/assist-id":
					fmt.Fprint(w, `{"name":"Assist"}`)
				case "POST /sessions/session-1/actions/compaction.compact":
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["agent_id"] != "assist-id" || body["model_ref"] != "openai/gpt-6.1-sol" || body["request_id"] == "" || body["message"] != "" {
						t.Errorf("incorrect compaction request: %v", body)
					}
					requests = append(requests, body["request_id"])
					if scenario == "rejected" {
						w.WriteHeader(404)
						fmt.Fprint(w, `{"error":{"code":"not_found","message":"action not found"}}`)
						return
					}
					if len(requests) > 1 {
						w.WriteHeader(409)
						fmt.Fprint(w, `{"error":{"code":"conflict","message":"agent changed"}}`)
						return
					}
					w.WriteHeader(202)
					fmt.Fprint(w, `{"run_id":"compact-run"}`)
				case "GET /sessions/session-1/runs":
					fmt.Fprintf(w, `[{"id":"compact-run","request_id":%q,"kind":"action","action":"compaction.compact"}]`, requests[0])
				case "GET /sessions/session-1/events":
					w.Header().Set("Content-Type", "text/event-stream")
					status := "completed"
					if scenario == "failed" || scenario == "aborted" {
						status = scenario
					}
					sendEvent(w, 1, "session.run."+status, map[string]string{"run_id": "compact-run", "error_message": "Cannot compact"})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "state.json")
			file, current, err := openState(path, "scope")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { file.close() }()
			b, tg := testBot()
			b.state, b.save, b.wingman = current, file.save, testWingman(t, server)
			b.state.SessionID = "session-1"
			if err := b.handle(context.Background(), privateUpdate(1, "/compact")); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "restart") {
				if scenario == "restart accepted" {
					tg.err = errors.New("Telegram unavailable")
				} else {
					b.save = func(s state) error {
						if s.Pending.RunID != "" {
							return errors.New("save failed after action accepted")
						}
						return file.save(s)
					}
				}
				if err := b.finish(context.Background()); err == nil {
					t.Fatal("expected interruption")
				}
				file.close()
				file, current, err = openState(path, "scope")
				if err != nil {
					t.Fatal(err)
				}
				b, tg = testBot()
				b.state, b.save, b.wingman = current, file.save, testWingman(t, server)
			}
			if err := b.finish(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 1
			if scenario == "restart uncertain" {
				want = 2
			}
			if len(requests) != want || (want == 2 && requests[0] != requests[1]) || b.state.SessionID != "session-1" || b.state.Pending != nil {
				t.Fatalf("compaction lost identity or session: requests=%v state=%+v", requests, b.state)
			}
			last := tg.sent[len(tg.sent)-1]
			switch scenario {
			case "rejected":
				if !strings.Contains(last, "action not found") {
					t.Fatalf("missing rejection: %v", tg.sent)
				}
			case "failed", "aborted":
				if !strings.Contains(last, scenario) || strings.Contains(last, "Conversation compacted") {
					t.Fatalf("incorrect failure reply: %v", tg.sent)
				}
			default:
				if len(tg.sent) != 2 || !strings.HasPrefix(last, "Conversation compacted.") {
					t.Fatalf("missing completion: %v", tg.sent)
				}
			}
		})
	}
}

func TestAssistValidationRejectsBuild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/ready" {
			fmt.Fprint(w, `{"ready":true}`)
		} else {
			fmt.Fprint(w, `{"name":"Build"}`)
		}
	}))
	defer server.Close()
	if err := testWingman(t, server).validate(context.Background()); err == nil || !strings.Contains(err.Error(), "Assist agent") {
		t.Fatalf("accepted the wrong agent: %v", err)
	}
}
