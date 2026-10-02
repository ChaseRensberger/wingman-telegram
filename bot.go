package main

import (
	"context"
	"log"
	"strings"
	"time"
)

type messenger interface {
	updates(context.Context, int64, int) ([]update, error)
	send(context.Context, int64, string) error
}

type bot struct {
	telegram messenger
	userID   int64
	state    state
	save     func(state) error
}

func (b *bot) run(ctx context.Context) error {
	for ctx.Err() == nil {
		if b.state.Pending != nil {
			if err := b.finish(ctx); err != nil {
				return err
			}
		}
		if err := b.resetStaleOffset(time.Now()); err != nil {
			return err
		}
		updates, err := b.telegram.updates(ctx, b.state.Offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("%v; retrying in 3 seconds", err)
			if err := wait(ctx, 3*time.Second); err != nil {
				return err
			}
			continue
		}
		for _, update := range updates {
			if err := b.handle(ctx, update); err != nil {
				return err
			}
			if b.state.Pending != nil {
				if err := b.finish(ctx); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
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
	text := strings.TrimSpace(m.Text)
	response := "Request recieved."
	switch text {
	case "/start", "/help":
		response = "Send a text message and I will reply: Request recieved.\nThis bot does not connect to Wingman."
	case "":
		response = "Send a text message. Attachments are not supported."
	default:
		if strings.HasPrefix(text, "/") {
			response = "Unknown command. Use /help."
		}
	}
	b.state.Pending = &pendingReply{ChatID: m.Chat.ID, Text: response}
	b.state.Offset = u.ID + 1
	return b.save(b.state)
}

func (b *bot) finish(ctx context.Context) error {
	p := b.state.Pending
	if err := b.telegram.send(ctx, p.ChatID, p.Text); err != nil {
		return err
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
