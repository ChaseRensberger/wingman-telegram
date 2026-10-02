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
	"unicode/utf16"

	"github.com/chaserensberger/wingman/client"
)

type fakeWingman struct {
	created     int
	requests    []string
	resultErr   error
	validateErr error
	resultFn    func(context.Context, string, *pendingReply, func() error) (string, error)
}

func (w *fakeWingman) validate(context.Context) error { return w.validateErr }

func (w *fakeWingman) createSession(context.Context) (string, error) {
	w.created++
	return "session-1", nil
}
func (w *fakeWingman) admit(_ context.Context, _ string, p *pendingReply) (string, error) {
	w.requests = append(w.requests, p.RequestID)
	return "run-1", nil
}
func (w *fakeWingman) result(ctx context.Context, session string, p *pendingReply, save func() error) (string, error) {
	if w.resultFn != nil {
		return w.resultFn(ctx, session, p, save)
	}
	return "Done", w.resultErr
}

func testWingman(t *testing.T, server *httptest.Server) *wingman {
	t.Helper()
	w := &wingman{origin: server.URL, username: "wingman", password: "secret", agentID: "build-id", modelRef: "openai/gpt-6.1-sol", workdir: "/srv/projects"}
	if err := w.connect(); err != nil {
		t.Fatal(err)
	}
	return w
}

func sendEvent(w http.ResponseWriter, seq int64, eventType string, data any) {
	event := map[string]any{"type": eventType, "data": data}
	if seq > 0 {
		event["cursor"] = map[string]any{"session_id": "session-1", "seq": seq}
	}
	encoded, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, encoded)
}

func messageEvent(runID, messageID, text string, revision int) map[string]any {
	return map[string]any{"run_id": runID, "message": map[string]any{
		"id": messageID, "revision": revision, "role": "assistant", "state": "completed",
		"content": []map[string]string{{"type": "text", "text": text}, {"type": "reasoning", "reasoning": "private"}},
	}}
}

func TestWingmanHTTPContract(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		username, password, ok := r.BasicAuth()
		if !ok || username != "wingman" || password != "secret" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("X-Wingman-Client") != "" {
			t.Error("must share Console's default identity")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /ready":
			fmt.Fprint(w, `{"ready":true,"version":"0.1.65"}`)
		case "GET /agents/build-id":
			fmt.Fprint(w, `{"name":"Build"}`)
		case "POST /sessions":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["working_directory"] != "/srv/projects" {
				t.Errorf("wrong workdir: %v", body)
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":"session-1"}`)
		case "POST /sessions/session-1/message":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["agent_id"] != "build-id" || body["model_ref"] != "openai/gpt-6.1-sol" || body["message"] != "  Implement feature\n" || body["request_id"] == "" {
				t.Errorf("wrong admission: %v", body)
			}
			requests = append(requests, body["request_id"])
			w.WriteHeader(202)
			fmt.Fprint(w, `{"run_id":"run-1"}`)
		case "GET /sessions/session-1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			sendEvent(w, 1, "session.message.created", messageEvent("old-run", "old", "old reply", 1))
			sendEvent(w, 2, "session.message.created", messageEvent("run-1", "reply", "Preview: https://demo.example", 2))
			sendEvent(w, 3, "session.message.created", messageEvent("run-1", "reply", "stale revision", 1))
			sendEvent(w, 4, "session.message.created", messageEvent("console-run", "other", "Console reply", 1))
			sendEvent(w, 5, "session.run.completed", map[string]string{"run_id": "console-run"})
			sendEvent(w, 6, "session.run.completed", map[string]string{"run_id": "run-1"})
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := testWingman(t, server)
	ctx := context.Background()
	if err := client.validate(ctx); err != nil {
		t.Fatal(err)
	}
	b, tg := testBot()
	b.wingman = client
	if err := b.handle(ctx, privateUpdate(1, "  Implement feature\n")); err != nil {
		t.Fatal(err)
	}
	if err := b.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || len(tg.sent) != 2 || tg.sent[1] != "Preview: https://demo.example" || !strings.Contains(tg.sent[0], "/console/sessions/session-1") {
		t.Fatalf("unexpected replies: %v", tg.sent)
	}
	client.password = "bad"
	if err := client.connect(); err != nil {
		t.Fatal(err)
	}
	if err := client.validate(ctx); err == nil {
		t.Fatal("accepted invalid credentials")
	}
}

func TestRestartDoesNotReadmitAcceptedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	b, tg := testBot()
	w := &fakeWingman{resultErr: errors.New("disconnected")}
	b.wingman, b.state, b.save = w, current, file.save
	ctx := context.Background()
	if err := b.handle(ctx, privateUpdate(1, "Build feature")); err != nil {
		t.Fatal(err)
	}
	if err := b.finish(ctx); err == nil {
		t.Fatal("expected disconnect")
	}
	file.close()
	file, restored, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	w.resultErr = nil
	w.validateErr = errors.New("agent unavailable after admission")
	restarted, newTG := testBot()
	restarted.wingman, restarted.state, restarted.save = w, restored, file.save
	if err := restarted.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if w.created != 1 || len(w.requests) != 1 || len(tg.sent) != 1 || !reflect.DeepEqual(newTG.sent, []string{"Done"}) {
		t.Fatalf("restart repeated work: %+v %v", w, newTG.sent)
	}
}

func TestAdmissionRetryUsesSameRequestID(t *testing.T) {
	b, _ := testBot()
	w := b.wingman.(*fakeWingman)
	ctx := context.Background()
	if err := b.handle(ctx, privateUpdate(9, "Build feature")); err != nil {
		t.Fatal(err)
	}
	saves := 0
	b.save = func(state) error {
		saves++
		if saves == 2 {
			return errors.New("disk failed after acceptance")
		}
		return nil
	}
	if err := b.finish(ctx); err == nil {
		t.Fatal("expected save failure")
	}
	// Restore the last durable state: request and session, but no run ID.
	b.state.Pending.RunID = ""
	b.save = func(state) error { return nil }
	if err := b.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.requests) != 2 || w.requests[0] != w.requests[1] {
		t.Fatal("retry changed request identity")
	}
}

func TestMessageChunks(t *testing.T) {
	text := strings.Repeat("界😀a", 4000)
	chunks := messageChunks(text)
	if strings.Join(chunks, "") != text {
		t.Fatal("text changed")
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > 4000 {
			t.Fatal("oversized chunk")
		}
	}
}

func TestBlankReplyChunksDoNotBlockDelivery(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("a", 4000) + "\n",
		strings.Repeat("a", 4000) + strings.Repeat(" ", 4000) + "Done",
		" \n\t",
		"",
	} {
		t.Run(fmt.Sprintf("length_%d", len(text)), func(t *testing.T) {
			b, tg := testBot()
			b.wingman.(*fakeWingman).resultFn = func(context.Context, string, *pendingReply, func() error) (string, error) {
				return text, nil
			}
			ctx := context.Background()
			if err := b.handle(ctx, privateUpdate(1, "Task")); err != nil {
				t.Fatal(err)
			}
			if err := b.finish(ctx); err != nil {
				t.Fatal(err)
			}
			if len(tg.sent) < 2 || b.state.Pending != nil {
				t.Fatalf("missing completed reply: %v", tg.sent)
			}
			for _, reply := range tg.sent {
				if strings.TrimSpace(reply) == "" {
					t.Fatalf("sent a blank chunk: %q", tg.sent)
				}
			}
			if strings.TrimSpace(text) == "" && tg.sent[1] != "Wingman completed the task without a text reply." {
				t.Fatalf("missing empty-reply fallback: %v", tg.sent)
			}
		})
	}
}

func TestTerminalRunRepliesAndNoOldText(t *testing.T) {
	for _, status := range []string{"failed", "aborted", "completed"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				sendEvent(w, 1, "session.message.created", messageEvent("old-run", "old", "old result", 1))
				sendEvent(w, 2, "session.run."+status, map[string]string{"run_id": "run", "error_message": "Task stopped"})
			}))
			defer server.Close()
			w := testWingman(t, server)
			text, err := w.result(context.Background(), "session-1", &pendingReply{RunID: "run"}, func() error { return nil })
			if err != nil || text == "old result" || text == "" {
				t.Fatalf("unexpected result: %q %v", text, err)
			}
			if status != "completed" && !strings.Contains(text, status) {
				t.Fatal("missing failure status")
			}
		})
	}
}

func TestFollowupReusesSession(t *testing.T) {
	b, _ := testBot()
	w := b.wingman.(*fakeWingman)
	for i, text := range []string{"Build feature", "Change its color"} {
		if err := b.handle(context.Background(), privateUpdate(int64(i+1), text)); err != nil {
			t.Fatal(err)
		}
		if err := b.finish(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if w.created != 1 || len(w.requests) != 2 || w.requests[0] == w.requests[1] {
		t.Fatal("followup did not reuse the session with a new request ID")
	}
}

func TestModelConfigurationIsRequired(t *testing.T) {
	t.Setenv("WINGMAN_URL", "https://wingman.example")
	t.Setenv("WINGMAN_PASSWORD", "secret")
	t.Setenv("WINGMAN_AGENT_ID", "build-id")
	t.Setenv("WINGMAN_WORKDIR", "/srv/projects")
	t.Setenv("WINGMAN_MODEL_REF", "")
	if _, _, err := configuredWingman(); err == nil {
		t.Fatal("missing explicit model accepted")
	}
	t.Setenv("WINGMAN_MODEL_REF", "openai/gpt-6.1-sol")
	t.Setenv("WINGMAN_CONSOLE_URL", "https://console.example")
	w, console, err := configuredWingman()
	if err != nil || console != "https://console.example" || w.modelRef != "openai/gpt-6.1-sol" {
		t.Fatalf("incorrect configuration: %v", err)
	}
}

func TestAdmissionHTTPFailures(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 413, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					fmt.Fprint(w, `[]`)
					return
				}
				w.Header().Set("X-Request-ID", "diagnostic-1")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"code":"validation_failed","message":"Unknown model; password=secret"}}`)
			}))
			defer server.Close()
			w := testWingman(t, server)
			_, err := w.admit(context.Background(), "session", &pendingReply{Text: "Task", RequestID: "request"})
			var rejection *admissionRejected
			if !errors.As(err, &rejection) {
				t.Fatalf("incorrect admission classification: %v", err)
			}
			var responseErr *client.APIError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != status || !strings.Contains(err.Error(), "Unknown model") || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "diagnostic-1") {
				t.Fatalf("lost error details or exposed credentials: %v", err)
			}
		})
	}
}

func TestRejectedAdmissionReplySurvivesRestart(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/ready" {
			fmt.Fprint(w, `{"ready":true}`)
			return
		}
		if r.URL.Path == "/agents/build-id" {
			fmt.Fprint(w, `{"name":"Build"}`)
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, `{"history":[]}`)
			return
		}
		requests++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"invalid_request","message":"unknown model: missing/model"}}`)
	}))
	defer server.Close()
	client := testWingman(t, server)
	path := filepath.Join(t.TempDir(), "state.json")
	file, current, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	b, tg := testBot()
	b.state, b.save, b.wingman = current, file.save, client
	b.state.SessionID = "session"
	ctx := context.Background()
	if err := b.handle(ctx, privateUpdate(1, "Task")); err != nil {
		t.Fatal(err)
	}
	tg.err = errors.New("Telegram disconnected")
	if err := b.finish(ctx); !errors.Is(err, tg.err) {
		t.Fatalf("expected persisted rejection followed by delivery failure, got %v", err)
	}
	file.close()
	file, restored, err := openState(path, "scope")
	if err != nil {
		t.Fatal(err)
	}
	defer file.close()
	if restored.Pending == nil || len(restored.Pending.Replies) != 1 || restored.Pending.RunID != "" {
		t.Fatalf("rejection was not persisted: %+v", restored.Pending)
	}
	restarted, newTG := testBot()
	restarted.state, restarted.save, restarted.wingman = restored, file.save, client
	if err := restarted.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.handle(ctx, privateUpdate(2, "/help")); err != nil {
		t.Fatal(err)
	}
	if err := restarted.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(newTG.sent) != 2 || !strings.Contains(newTG.sent[0], "unknown model") || !strings.Contains(newTG.sent[1], "Build agent") || restarted.state.Pending != nil {
		t.Fatalf("rejection was retried or blocked subsequent commands: requests=%d replies=%v", requests, newTG.sent)
	}
}

func TestUncertainAdmissionKeepsPendingTask(t *testing.T) {
	for _, status := range []int{200, 408, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/ready" {
					fmt.Fprint(w, `{"ready":true}`)
					return
				}
				if r.URL.Path == "/agents/build-id" {
					fmt.Fprint(w, `{"name":"Build"}`)
					return
				}
				if r.Method == "GET" {
					fmt.Fprint(w, `{"history":[]}`)
					return
				}
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				requests = append(requests, body["request_id"])
				w.WriteHeader(status)
				// An empty successful response also leaves admission uncertain.
			}))
			defer server.Close()
			b, tg := testBot()
			b.state.SessionID = "session"
			b.wingman = testWingman(t, server)
			ctx := context.Background()
			if err := b.handle(ctx, privateUpdate(1, "Task")); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				attempt, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
				err := b.finish(attempt)
				cancel()
				if err == nil {
					t.Fatal("uncertain admission was treated as a completed task")
				}
			}
			if b.state.Pending == nil || len(b.state.Pending.Replies) != 0 || len(tg.sent) != 0 || len(requests) != 2 || requests[0] == "" || requests[0] != requests[1] {
				t.Fatalf("uncertain task was not retained for an idempotent retry: pending=%+v replies=%v requests=%v", b.state.Pending, tg.sent, requests)
			}
		})
	}
}

func TestAdmissionConflictReconcilesSavedRequest(t *testing.T) {
	for _, scenario := range []string{"accepted", "not accepted", "different input", "lookup failed"} {
		t.Run(scenario, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "POST" {
					var request client.MessageSessionRequest
					_ = json.NewDecoder(r.Body).Decode(&request)
					if request.RequestId == nil || *request.RequestId != "saved-request" {
						t.Errorf("changed retry identity: %+v", request)
					}
					w.WriteHeader(409)
					fmt.Fprint(w, `{"error":{"code":"conflict","message":"effective agent changed"}}`)
					return
				}
				switch scenario {
				case "accepted":
					fmt.Fprint(w, `[{"id":"original-run","request_id":"saved-request","message":"Task","agent":{"id":"build-id","name":"Renamed Build","model_ref":"old/model"}}]`)
				case "not accepted":
					fmt.Fprint(w, `[]`)
				case "different input":
					fmt.Fprint(w, `[{"id":"different-run","request_id":"saved-request","message":"Different task"}]`)
				case "lookup failed":
					w.WriteHeader(403)
					fmt.Fprint(w, `{"error":{"code":"forbidden","message":"cannot inspect runs"}}`)
				}
			}))
			defer server.Close()
			w := testWingman(t, server)
			id, err := w.admit(context.Background(), "session-1", &pendingReply{Text: "Task", RequestID: "saved-request"})
			var rejected *admissionRejected
			if scenario == "accepted" {
				if err != nil || id != "original-run" {
					t.Fatalf("lost accepted run: %q %v", id, err)
				}
			} else if err == nil || errors.As(err, &rejected) != (scenario == "not accepted") {
				t.Fatalf("incorrect conflict handling: %v", err)
			}
			if !reflect.DeepEqual(paths, []string{"POST /sessions/session-1/message", "GET /sessions/session-1/runs"}) {
				t.Fatalf("unexpected reconciliation requests: %v", paths)
			}
		})
	}
}

func TestSDKRetryRetainsRequestAndHonorsRetryAfter(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			var ids []string
			var times []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var request client.MessageSessionRequest
				_ = json.NewDecoder(r.Body).Decode(&request)
				ids = append(ids, *request.RequestId)
				times = append(times, time.Now())
				if len(ids) == 1 {
					w.Header().Set("Retry-After", "2")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"try later"}}`)
					return
				}
				w.WriteHeader(202)
				fmt.Fprint(w, `{"run_id":"run-1"}`)
			}))
			defer server.Close()
			w := testWingman(t, server)
			timeout := 5 * time.Second
			if cancelEarly {
				timeout = 50 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			id, err := w.admit(ctx, "session-1", &pendingReply{RequestID: "saved-request", Text: "Task"})
			if cancelEarly {
				if !errors.Is(err, context.DeadlineExceeded) || len(ids) != 1 {
					t.Fatalf("retry ignored cancellation or Retry-After: ids=%v err=%v", ids, err)
				}
			} else if err != nil || id != "run-1" || !reflect.DeepEqual(ids, []string{"saved-request", "saved-request"}) || times[1].Sub(times[0]) < 2*time.Second {
				t.Fatalf("incorrect retry: id=%s requests=%v times=%v err=%v", id, ids, times, err)
			}
		})
	}
}

func TestReadinessPrecedesAgentValidation(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ready":false,"version":"0.1.65"}`)
	}))
	defer server.Close()
	w := testWingman(t, server)
	if err := w.validate(context.Background()); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("non-ready server accepted: %v", err)
	}
	if !reflect.DeepEqual(paths, []string{"/ready"}) {
		t.Fatalf("validated agent before readiness: %v", paths)
	}
}
