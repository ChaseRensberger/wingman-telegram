package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/chaserensberger/wingman/api"
	"github.com/chaserensberger/wingman/client"
	"github.com/chaserensberger/wingman/models"
)

func (w *wingman) result(ctx context.Context, sessionID string, p *pendingReply, save func() error) (string, error) {
	delay := time.Second
	for {
		if text, done := p.Events.result(); done {
			return text, nil
		}
		after := p.Events.Seq
		stream, err := w.streaming.StreamSessionEvents(ctx, sessionID, &client.SessionEventsOptions{After: &after})
		if ctx.Err() != nil {
			if stream != nil {
				stream.Close()
			}
			return "", ctx.Err()
		}
		if err != nil && !retryableWingman(err) {
			return "", w.failure(err)
		}
		resync := false
		for stream != nil && stream.Next() {
			event := stream.Event()
			if event.Type == api.SessionEventEventsResyncRequired {
				resync = true
				break
			}
			if event.Type == api.SessionEventEventsSynchronized {
				// A terminal run can predate the stream, including after recovery
				// from a server failure that prevented its message event being saved.
				if err := w.recoverResult(ctx, sessionID, p, save); err != nil {
					stream.Close()
					return "", err
				}
			} else if event.Cursor != nil && event.Cursor.SessionID == sessionID && event.Cursor.Seq > p.Events.Seq {
				p.Events.apply(event, p.RunID)
				// Save the cursor and the output it represents in one checkpoint.
				if err := save(); err != nil {
					stream.Close()
					return "", err
				}
				delay = time.Second
			}
			if text, done := p.Events.result(); done {
				stream.Close()
				return text, nil
			}
		}
		if stream != nil {
			err = stream.Err()
			stream.Close()
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err := w.recoverResult(ctx, sessionID, p, save); err != nil {
			return "", err
		}
		if text, done := p.Events.result(); done {
			return text, nil
		}
		if resync {
			err = fmt.Errorf("Wingman requested session event resynchronization")
		} else if err == nil {
			err = io.EOF
		}
		if err := w.retryWait(ctx, err, delay); err != nil {
			return "", err
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (events *runEvents) apply(event api.SessionEvent, runID string) {
	events.Seq = event.Cursor.Seq
	switch data := event.Data.(type) {
	case *api.MessageCreatedEventData:
		if data.RunID != runID || data.Message.Role != models.RoleAssistant {
			return
		}
		message := replyMessage{ID: data.Message.ID, Revision: data.Message.Revision}
		if data.Message.State == models.MessageStateCompleted {
			var text []string
			for _, part := range data.Message.Content {
				if part, ok := part.(models.TextPart); ok && part.Text != "" {
					text = append(text, part.Text)
				}
			}
			message.Text = strings.Join(text, "\n")
		}
		events.put(message)
	case *api.RunEventData:
		if data.RunID != runID {
			return
		}
		switch event.Type {
		case api.SessionEventRunCompleted:
			events.Status = "completed"
		case api.SessionEventRunFailed:
			events.Status, events.Error = "failed", data.ErrorMessage
		case api.SessionEventRunAborted:
			events.Status, events.Error = "aborted", data.ErrorMessage
		}
	}
}

func (events *runEvents) put(message replyMessage) {
	for i, existing := range events.Messages {
		if existing.ID == message.ID {
			if message.Revision >= existing.Revision {
				events.Messages[i] = message
			}
			return
		}
	}
	events.Messages = append(events.Messages, message)
}

func (events runEvents) result() (string, bool) {
	switch events.Status {
	case "failed", "aborted":
		return strings.TrimSpace("Wingman task " + events.Status + ". " + events.Error), true
	case "completed":
		for i := len(events.Messages) - 1; i >= 0; i-- {
			if events.Messages[i].Text != "" {
				return events.Messages[i].Text, true
			}
		}
		return "Wingman completed the task without a text reply.", true
	}
	return "", false
}

func (w *wingman) recoverResult(ctx context.Context, sessionID string, p *pendingReply, save func() error) error {
	run, err := wingmanRequest(ctx, w, func() (*client.GetSessionRunHTTPResponse, error) {
		return w.sdk.GetSessionRunWithResponse(ctx, sessionID, p.RunID, nil)
	})
	if err != nil {
		return err
	}
	if run.JSON200 == nil {
		return fmt.Errorf("Wingman returned no run during event recovery")
	}
	// Read the run first so a terminal status implies that the subsequent
	// transcript snapshot includes the run's final persisted messages.
	session, err := wingmanRequest(ctx, w, func() (*client.GetSessionHTTPResponse, error) {
		return w.sdk.GetSessionWithResponse(ctx, sessionID, nil)
	})
	if err != nil {
		return err
	}
	if session.JSON200 == nil || session.JSON200.History == nil {
		return fmt.Errorf("Wingman returned no session history during event recovery")
	}
	switch run.JSON200.Status {
	case "queued", "running":
		return nil
	case "completed":
		calls, err := wingmanRequest(ctx, w, func() (*client.ListSessionModelCallsHTTPResponse, error) {
			return w.sdk.ListSessionModelCallsWithResponse(ctx, sessionID, nil)
		})
		if err != nil {
			return err
		}
		if calls.JSON200 == nil {
			return fmt.Errorf("Wingman returned no model calls during event recovery")
		}
		ids := map[string]bool{}
		for _, call := range *calls.JSON200 {
			if call.RunId != nil && *call.RunId == p.RunID && call.AssistantMessageId != nil {
				ids[*call.AssistantMessageId] = true
			}
		}
		p.Events.Messages = nil
		for _, message := range *session.JSON200.History {
			if message.Id == nil || !ids[*message.Id] || message.Role != "assistant" || message.State == nil || *message.State != "completed" {
				continue
			}
			var text []string
			for _, part := range message.Content {
				part, err := part.AsTextPart()
				if err != nil {
					return fmt.Errorf("decode Wingman message part: %w", err)
				}
				if part.Type == "text" && part.Text != "" {
					text = append(text, part.Text)
				}
			}
			var revision int64
			if message.Revision != nil {
				revision = *message.Revision
			}
			p.Events.put(replyMessage{ID: *message.Id, Revision: revision, Text: strings.Join(text, "\n")})
		}
	case "failed", "aborted":
	default:
		return fmt.Errorf("unknown Wingman run status %q", run.JSON200.Status)
	}
	p.Events.Status = run.JSON200.Status
	if run.JSON200.ErrorMessage != nil {
		p.Events.Error = *run.JSON200.ErrorMessage
	}
	return save()
}
