package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("wingman-telegram", flag.ContinueOnError)
	statePath := flags.String("state", defaultStatePath(), "Path for persistent chat state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 || flags.NArg() == 1 && flags.Arg(0) != "identify" {
		return fmt.Errorf("usage: wingman-telegram [-state path] [identify]")
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("set TELEGRAM_BOT_TOKEN outside Git")
	}
	tg := newTelegram(token)
	if flags.Arg(0) == "identify" {
		updates, err := tg.updates(ctx, 0, 0)
		if err != nil {
			return err
		}
		seen := map[int64]bool{}
		for _, u := range updates {
			if m := u.Message; m != nil && m.From != nil && m.Chat.Type == "private" && !seen[m.From.ID] {
				fmt.Printf("Telegram user ID: %d (@%s)\n", m.From.ID, m.From.Username)
				seen[m.From.ID] = true
			}
		}
		if len(seen) == 0 {
			return fmt.Errorf("send /start to your bot, then run identify again")
		}
		return nil
	}
	userID, err := strconv.ParseInt(os.Getenv("TELEGRAM_USER_ID"), 10, 64)
	if err != nil || userID <= 0 {
		return fmt.Errorf("set TELEGRAM_USER_ID to your numeric Telegram user ID; use identify to find it")
	}
	w, consoleURL, err := configuredWingman()
	if err != nil {
		return err
	}
	me, err := tg.me(ctx)
	if err != nil {
		return err
	}
	scope := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("telegram-reply\n%d\n%d", me.ID, userID))))
	file, current, err := openState(*statePath, scope)
	if err != nil {
		return err
	}
	defer file.close()
	target := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%s", w.origin, w.agentID, w.modelRef, w.workdir))))
	if current.Target != "" && current.Target != target {
		return fmt.Errorf("state belongs to a different Wingman configuration; use a separate -state path")
	}
	current.Target = target
	if err := file.save(current); err != nil {
		return err
	}
	log.Printf("Bot @%s is ready for Telegram user %d. State: %s", me.Username, userID, *statePath)
	b := &bot{telegram: tg, userID: userID, botID: me.ID, wingman: w, consoleURL: consoleURL, state: current, save: file.save}
	if err := b.run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%w; state is preserved, restart the client to resume reply delivery", err)
	}
	return ctx.Err()
}

func defaultStatePath() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "wingman-telegram", "state.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "wingman-telegram", "state.json")
}
