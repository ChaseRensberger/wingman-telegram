package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chaserensberger/wingman/api"
	"github.com/chaserensberger/wingman/client"
)

type wingman struct {
	origin, username, password, agentID, modelRef, workdir string
	sdk, streaming                                         *client.SDK
}

type wingmanError struct {
	message string
	cause   error
}

func (e *wingmanError) Error() string { return e.message }
func (e *wingmanError) Unwrap() error { return e.cause }

type admissionRejected struct{ cause error }

func (e *admissionRejected) Error() string { return e.cause.Error() }
func (e *admissionRejected) Unwrap() error { return e.cause }

func configuredWingman() (*wingman, string, error) {
	w := &wingman{origin: os.Getenv("WINGMAN_URL"), username: os.Getenv("WINGMAN_USERNAME"), password: os.Getenv("WINGMAN_PASSWORD"), agentID: os.Getenv("WINGMAN_AGENT_ID"), modelRef: os.Getenv("WINGMAN_MODEL_REF"), workdir: os.Getenv("WINGMAN_WORKDIR")}
	if w.username == "" {
		w.username = "wingman"
	}
	origin, err := originURL(w.origin)
	if err != nil {
		return nil, "", fmt.Errorf("WINGMAN_URL: %w", err)
	}
	w.origin = origin
	if w.password == "" || w.agentID == "" || w.modelRef == "" || !filepath.IsAbs(w.workdir) {
		return nil, "", fmt.Errorf("set WINGMAN_PASSWORD, WINGMAN_AGENT_ID (Build), WINGMAN_MODEL_REF (GPT 6.1 Sol), and an absolute WINGMAN_WORKDIR on the server")
	}
	console := os.Getenv("WINGMAN_CONSOLE_URL")
	if console == "" {
		console = w.origin
	}
	console, err = originURL(console)
	if err != nil {
		return nil, "", fmt.Errorf("WINGMAN_CONSOLE_URL: %w", err)
	}
	if err := w.connect(); err != nil {
		return nil, "", err
	}
	return w, console, nil
}

func originURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("use an HTTP or HTTPS origin without credentials or a path")
	}
	return strings.TrimRight(raw, "/"), nil
}

func (w *wingman) connect() error {
	var err error
	w.sdk, err = client.New(w.origin, client.WithBasicAuth(w.username, w.password), client.WithTransport(&http.Client{Timeout: 40 * time.Second}))
	if err != nil {
		return err
	}
	// SSE has no whole-response deadline. Headers and connection setup still
	// have timeouts, and cancellation closes an idle stream during shutdown.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 40 * time.Second
	w.streaming, err = client.New(w.origin, client.WithBasicAuth(w.username, w.password), client.WithTransport(&http.Client{Transport: transport}))
	return err
}

func (w *wingman) validate(ctx context.Context) error {
	ready, err := wingmanRequest(ctx, w, func() (*client.GetReadinessHTTPResponse, error) {
		return w.sdk.GetReadinessWithResponse(ctx, nil)
	})
	if err != nil {
		return err
	}
	if ready.JSON200 == nil || !ready.JSON200.Ready {
		return fmt.Errorf("Wingman is not ready")
	}
	log.Printf("Wingman server version: %s", ready.JSON200.Version)
	agent, err := wingmanRequest(ctx, w, func() (*client.GetAgentHTTPResponse, error) {
		return w.sdk.GetAgentWithResponse(ctx, w.agentID, nil)
	})
	if err != nil {
		return err
	}
	if agent.JSON200 == nil {
		return fmt.Errorf("Wingman returned no agent")
	}
	if agent.JSON200.Name != "Build" {
		return fmt.Errorf("WINGMAN_AGENT_ID must select the Build agent, got %q", agent.JSON200.Name)
	}
	return nil
}

func (w *wingman) createSession(ctx context.Context) (string, error) {
	title := "Telegram"
	// Session creation has no request ID, so do not automatically retry it.
	result, err := w.sdk.CreateSessionWithResponse(ctx, nil, client.CreateSessionRequest{Title: &title, WorkingDirectory: &w.workdir})
	if err != nil {
		return "", w.failure(err)
	}
	if result.JSON201 == nil || result.JSON201.Id == "" {
		return "", fmt.Errorf("Wingman returned an empty session ID")
	}
	return result.JSON201.Id, nil
}

func (w *wingman) admit(ctx context.Context, id string, p *pendingReply) (string, error) {
	if p.RequestID == "" {
		return "", fmt.Errorf("Wingman admission requires a persisted request ID")
	}
	request := client.NewMessageAdmission(client.MessageSessionRequest{AgentId: w.agentID, ModelRef: &w.modelRef, Message: p.Text, RequestId: &p.RequestID})
	result, err := wingmanRequest(ctx, w, func() (client.MessageSessionResponse, error) {
		return w.sdk.AdmitMessage(ctx, id, request)
	})
	var responseErr *client.APIError
	if errors.As(err, &responseErr) {
		if responseErr.Response.Code == api.ErrorCodeConflict || responseErr.StatusCode == http.StatusConflict {
			// The first attempt may already exist with an older effective agent
			// or session location. Request IDs are scoped to this session.
			runs, lookupErr := wingmanRequest(ctx, w, func() (*client.ListSessionRunsHTTPResponse, error) {
				return w.sdk.ListSessionRunsWithResponse(ctx, id, nil)
			})
			if lookupErr != nil {
				return "", lookupErr
			}
			if runs.JSON200 == nil {
				return "", fmt.Errorf("Wingman returned no runs while reconciling request %s", p.RequestID)
			}
			for _, run := range *runs.JSON200 {
				if run.RequestId != nil && *run.RequestId == p.RequestID {
					if run.Message != p.Text || run.Id == "" {
						return "", fmt.Errorf("Wingman request %s conflicts with a different saved input", p.RequestID)
					}
					return run.Id, nil
				}
			}
			return "", &admissionRejected{cause: err}
		}
		switch responseErr.Response.Code {
		case api.ErrorCodeInvalidRequest, api.ErrorCodeUnauthorized, api.ErrorCodeForbidden,
			api.ErrorCodeNotFound, api.ErrorCodeMethodNotAllowed, api.ErrorCodePayloadTooLarge,
			api.ErrorCodeUnsupportedMedia, api.ErrorCodeValidationFailed:
			return "", &admissionRejected{cause: err}
		}
	}
	if err == nil && result.RunId == "" {
		err = fmt.Errorf("Wingman returned an empty run ID")
	}
	return result.RunId, err
}

func wingmanRequest[T any](ctx context.Context, w *wingman, request func() (T, error)) (T, error) {
	delay := time.Second
	for {
		result, err := request()
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !retryableWingman(err) {
			return result, w.failure(err)
		}
		if err := w.retryWait(ctx, err, delay); err != nil {
			return result, err
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func retryableWingman(err error) bool {
	var response *client.APIError
	if errors.As(err, &response) {
		return response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500
	}
	var connection *url.Error
	return errors.As(err, &connection) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (w *wingman) retryWait(ctx context.Context, err error, delay time.Duration) error {
	var response *client.APIError
	if errors.As(err, &response) {
		delay = max(delay, response.RetryAfter)
	}
	log.Printf("%v; retrying in %s", w.failure(err), delay)
	return wait(ctx, delay)
}

func (w *wingman) failure(err error) error {
	message := err.Error()
	var response *client.APIError
	if errors.As(err, &response) && response.RequestID != "" {
		message += " (request " + response.RequestID + ")"
	}
	if w.password != "" {
		message = strings.ReplaceAll(message, w.password, "[redacted]")
	}
	return &wingmanError{message: message, cause: err}
}

func messageChunks(text string) []string {
	var chunks []string
	start, units := 0, 0
	for i, r := range text {
		size := 1
		if r > 0xffff {
			size = 2
		}
		if units+size > 4000 {
			if strings.TrimSpace(text[start:i]) != "" {
				chunks = append(chunks, text[start:i])
			}
			start, units = i, 0
		}
		units += size
	}
	if strings.TrimSpace(text[start:]) != "" {
		chunks = append(chunks, text[start:])
	}
	if len(chunks) == 0 {
		return []string{"Wingman completed the task without a text reply."}
	}
	return chunks
}
