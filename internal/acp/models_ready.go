package acp

import (
	"context"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
)

// modelsReadyWait bounds how long session/new, session/load and
// session/resume wait for the background model discovery before answering
// with the models known so far. A variable so tests can shorten it.
var modelsReadyWait = 2 * time.Second

// awaitModels waits until the background model discovery has finished
// (Options.ModelsReady closed), ctx ends, or modelsReadyWait passes, so the
// model selector in a session response lists local endpoint and subscription
// models when they answer promptly. initialize never waits.
//
// When the wait gives up, the late models are still delivered: the first
// timeout starts a single goroutine that waits for discovery to finish and
// then sends every open session a config_option_update with the full list.
func (b *bridge) awaitModels(ctx context.Context) {
	ready := b.opts.ModelsReady
	if ready == nil {
		return
	}
	timer := time.NewTimer(modelsReadyWait)
	defer timer.Stop()
	select {
	case <-ready:
		return
	case <-ctx.Done():
	case <-timer.C:
	}
	b.lateModels.Do(func() {
		go func() {
			<-ready
			b.broadcastConfigOptions()
		}()
	})
}

// broadcastConfigOptions sends every open session its current config
// options (the model list among them), read against fresh config.
func (b *bridge) broadcastConfigOptions() {
	if b.conn == nil {
		return
	}
	cfg := b.opts.Cfg
	if fresh, err := config.LoadFull(); err == nil {
		cfg = fresh
	}
	type update struct {
		sid     acpsdk.SessionId
		options []acpsdk.SessionConfigOption
	}
	b.mu.Lock()
	updates := make([]update, 0, len(b.sessions))
	for id, s := range b.sessions {
		updates = append(updates, update{acpsdk.SessionId(id), buildConfigOptions(s, &cfg, b.opts.Providers)})
	}
	b.mu.Unlock()
	for _, u := range updates {
		_ = b.conn.SessionUpdate(context.Background(), acpsdk.SessionNotification{
			SessionId: u.sid,
			Update: acpsdk.SessionUpdate{ConfigOptionUpdate: &acpsdk.SessionConfigOptionUpdate{
				ConfigOptions: u.options,
			}},
		})
	}
}
