package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type messenger interface {
	updates(context.Context, int64, int) ([]update, error)
	send(context.Context, int64, string) error
}

type bot struct {
	telegram   messenger
	userID     int64
	state      state
	save       func(state) error
	wingman    runtime
	botID      int64
	consoleURL string
}

type runtime interface {
	validate(context.Context) error
	createSession(context.Context) (string, error)
	admit(context.Context, string, *pendingReply) (string, error)
	result(context.Context, string, *pendingReply, func() error) (string, error)
}

func (b *bot) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	polled := make(chan []update, 1)
	finished := make(chan error, 1)
	checkpoints := make(chan taskCheckpoint)
	polling, working := false, false
	for ctx.Err() == nil {
		if !working {
			if b.state.Pending == nil && len(b.state.Queue) > 0 {
				b.prepare(b.state.Queue[0])
				b.state.Queue = b.state.Queue[1:]
				if err := b.save(b.state); err != nil {
					return err
				}
			}
			if b.state.Pending != nil {
				// The worker owns a task snapshot. Only this loop writes durable
				// state, merging task checkpoints with newly queued updates.
				worker := *b
				worker.state = state{Pending: clonePending(b.state.Pending), SessionID: b.state.SessionID, EventSeq: b.state.EventSeq}
				worker.save = func(s state) error {
					checkpoint := taskCheckpoint{pending: clonePending(s.Pending), sessionID: s.SessionID, eventSeq: s.EventSeq, saved: make(chan error, 1)}
					select {
					case checkpoints <- checkpoint:
					case <-ctx.Done():
						return ctx.Err()
					}
					select {
					case err := <-checkpoint.saved:
						return err
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				working = true
				workers.Add(1)
				go func() {
					defer workers.Done()
					finished <- worker.finish(ctx)
				}()
			}
		}
		if !polling {
			if err := b.resetStaleOffset(time.Now()); err != nil {
				return err
			}
			offset := b.state.Offset
			polling = true
			workers.Add(1)
			go func() {
				defer workers.Done()
				updates, err := b.telegram.updates(ctx, offset, 30)
				if err != nil {
					updates = nil
					if ctx.Err() == nil {
						log.Printf("%v; retrying in 3 seconds", err)
						_ = wait(ctx, 3*time.Second)
					}
				}
				polled <- updates
			}()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case checkpoint := <-checkpoints:
			b.state.Pending, b.state.SessionID = checkpoint.pending, checkpoint.sessionID
			b.state.EventSeq = checkpoint.eventSeq
			err := b.save(b.state)
			checkpoint.saved <- err
			if err != nil {
				return err
			}
		case err := <-finished:
			working = false
			if err != nil {
				return err
			}
		case updates := <-polled:
			polling = false
			for _, update := range updates {
				if err := b.handle(ctx, update); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

type taskCheckpoint struct {
	pending   *pendingReply
	sessionID string
	eventSeq  int64
	saved     chan error
}

func clonePending(p *pendingReply) *pendingReply {
	if p == nil {
		return nil
	}
	copy := *p
	copy.Replies = append([]string(nil), p.Replies...)
	copy.Events.Messages = append([]replyMessage(nil), p.Events.Messages...)
	return &copy
}

func (b *bot) resetStaleOffset(now time.Time) error {
	// Telegram retains updates for at most 24 hours and can randomize IDs
	// after a week without updates. Reset before polling, once previously
	// processed updates have expired, so a lower new ID is not confirmed away.
	if b.state.Offset == 0 || now.Sub(time.Unix(b.state.LastUpdateAt, 0)) < 24*time.Hour {
		return nil
	}
	b.state.Offset = 0
	b.state.LastUpdateAt = now.Unix()
	return b.save(b.state)
}

func (b *bot) handle(ctx context.Context, u update) error {
	if u.ID < b.state.Offset {
		return nil
	}
	b.state.LastUpdateAt = time.Now().Unix()
	m := u.Message
	if m == nil || m.From == nil || m.From.ID != b.userID || m.Chat.Type != "private" || m.Chat.ID != b.userID {
		b.state.Offset = u.ID + 1
		return b.save(b.state)
	}
	if b.state.Pending != nil || len(b.state.Queue) > 0 {
		b.state.Queue = append(b.state.Queue, u)
	} else {
		b.prepare(u)
	}
	b.state.Offset = u.ID + 1
	return b.save(b.state)
}

func (b *bot) prepare(u update) {
	m := u.Message
	text := strings.TrimSpace(m.Text)
	response := ""
	switch text {
	case "/start", "/help":
		response = "Send a task to Wingman's Build agent using GPT 6.1 Sol.\n/session shows the Console link. Approve tool requests in Console."
	case "/session":
		response = "Send a task first to create a session."
		if b.state.SessionID != "" {
			response = b.sessionURL()
		}
	case "":
		response = "Send a text message. Attachments are not supported."
	default:
		if strings.HasPrefix(text, "/") {
			response = "Unknown command. Use /help."
		}
	}
	b.state.Pending = &pendingReply{ChatID: m.Chat.ID, Text: response}
	if response == "" {
		b.state.Pending.Text = m.Text
		b.state.Pending.RequestID = fmt.Sprintf("telegram:%d:%d:%d", b.botID, m.Chat.ID, u.ID)
		b.state.Pending.Events.Seq = b.state.EventSeq
	}
}

func (b *bot) finish(ctx context.Context) error {
	p := b.state.Pending
	if p.RequestID != "" {
		return b.finishTask(ctx)
	}
	if err := b.telegram.send(ctx, p.ChatID, p.Text); err != nil {
		return err
	}
	b.state.Pending = nil
	return b.save(b.state)
}

func (b *bot) sessionURL() string {
	return b.consoleURL + "/console/sessions/" + b.state.SessionID
}

func (b *bot) finishTask(ctx context.Context) error {
	p := b.state.Pending
	// Remote validation runs in the task worker so Telegram polling continues
	// during outages. Saved replies and accepted runs need no admission checks.
	if len(p.Replies) == 0 && p.RunID == "" {
		if err := b.wingman.validate(ctx); err != nil {
			return err
		}
	}
	if len(p.Replies) == 0 && b.state.SessionID == "" {
		id, err := b.wingman.createSession(ctx)
		if err != nil {
			return err
		}
		b.state.SessionID = id
		if err := b.save(b.state); err != nil {
			return err
		}
	}
	if len(p.Replies) == 0 && p.RunID == "" {
		id, err := b.wingman.admit(ctx, b.state.SessionID, p)
		if err != nil {
			var rejection *admissionRejected
			if !errors.As(err, &rejection) {
				return err
			}
			p.Replies = messageChunks("Wingman rejected your task. " + rejection.Error())
		} else {
			p.RunID = id
		}
		if err := b.save(b.state); err != nil {
			return err
		}
	}
	if len(p.Replies) == 0 {
		if !p.Notified {
			if err := b.telegram.send(ctx, p.ChatID, "Wingman accepted your task. Watch and approve tools here:\n"+b.sessionURL()); err != nil {
				return err
			}
			p.Notified = true
			if err := b.save(b.state); err != nil {
				return err
			}
		}
		text, err := b.wingman.result(ctx, b.state.SessionID, p, func() error {
			b.state.EventSeq = max(b.state.EventSeq, p.Events.Seq)
			return b.save(b.state)
		})
		if err != nil {
			return err
		}
		p.Replies = messageChunks(text)
		if err := b.save(b.state); err != nil {
			return err
		}
	}
	for p.Sent < len(p.Replies) {
		// Older state may contain whitespace-only chunks that Telegram rejects.
		if strings.TrimSpace(p.Replies[p.Sent]) != "" {
			if err := b.telegram.send(ctx, p.ChatID, p.Replies[p.Sent]); err != nil {
				return err
			}
		}
		p.Sent++
		if err := b.save(b.state); err != nil {
			return err
		}
	}
	b.state.Pending = nil
	return b.save(b.state)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
