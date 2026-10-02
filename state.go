package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type pendingReply struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

type state struct {
	Scope        string        `json:"scope"`
	Offset       int64         `json:"offset"`
	LastUpdateAt int64         `json:"last_update_at,omitempty"`
	Pending      *pendingReply `json:"pending,omitempty"`
}

type stateFile struct {
	path string
	lock *os.File
}

func openState(path, scope string) (*stateFile, state, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, state{}, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, state{}, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, state{}, fmt.Errorf("state is locked by another process: %s", path)
	}
	file := &stateFile{path: path, lock: lock}
	current := state{Scope: scope}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file, current, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &current)
	}
	if err == nil && current.Scope != scope {
		err = fmt.Errorf("state belongs to a different bot, user, or client version; use a different -state path")
	}
	if err == nil && current.Offset > 0 && current.LastUpdateAt == 0 {
		// Older state files did not record update times. Their last write is
		// a conservative estimate that preserves recent duplicate protection.
		var info os.FileInfo
		info, err = os.Stat(path)
		if err == nil {
			current.LastUpdateAt = info.ModTime().Unix()
		}
	}
	if err == nil && current.Pending != nil {
		p := current.Pending
		if p.ChatID <= 0 || p.Text == "" {
			err = fmt.Errorf("invalid pending reply in state")
		}
	}
	if err != nil {
		file.close()
		return nil, state{}, err
	}
	return file, current, nil
}

func (f *stateFile) save(current state) error {
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(f.path), ".telegram-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(temp.Name(), f.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(f.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (f *stateFile) close() {
	f.lock.Close()
}
