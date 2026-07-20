package main

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatui"
)

const tuiCloseTimeout = 10 * time.Second

type closeOnceChatBackend struct {
	chatpkg.Backend
	once sync.Once
	err  error
}

func (b *closeOnceChatBackend) Close(ctx context.Context) error {
	b.once.Do(func() { b.err = b.Backend.Close(ctx) })
	return b.err
}

func runTUIChat(ctx context.Context, backend chatpkg.Backend, open chatpkg.OpenOptions, in io.Reader, out io.Writer) (err error) {
	guarded := &closeOnceChatBackend{Backend: backend}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tuiCloseTimeout)
		defer cancel()
		if closeErr := guarded.Close(closeCtx); err == nil {
			err = closeErr
		}
	}()
	m := chatui.New(guarded, open, chatui.Options{NoColor: os.Getenv("NO_COLOR") != "", Context: ctx})
	_, err = tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	return err
}
