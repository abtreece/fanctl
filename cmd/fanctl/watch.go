package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// configWatcher calls onChange (debounced) when the config file is written.
// It watches the containing directory so atomic-rename saves (the common
// editor/`mv` pattern) are still caught.
type configWatcher struct {
	w        *fsnotify.Watcher
	path     string
	onChange func()
}

func newConfigWatcher(path string, onChange func()) (*configWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		_ = w.Close()
		return nil, err
	}
	return &configWatcher{w: w, path: filepath.Clean(path), onChange: onChange}, nil
}

func (cw *configWatcher) run(ctx context.Context) {
	const debounce = 200 * time.Millisecond
	var timer *time.Timer
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case ev, ok := <-cw.w.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != cw.path {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			// Coalesce bursts of events into a single reload.
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, cw.onChange)
		case _, ok := <-cw.w.Errors:
			if !ok {
				return
			}
		}
	}
}

func (cw *configWatcher) close() error { return cw.w.Close() }
