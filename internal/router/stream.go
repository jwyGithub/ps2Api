package router

import (
	"context"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// Stream runs a streaming completion through the shared retry/failover driver.
// A tracked emit callback records whether any delta has been flushed; once it
// has, the driver fails hard rather than retrying (retrying would duplicate
// output). While no delta has been sent it retries/fails over exactly like Chat.
func (r *Router) Stream(ctx context.Context, req *provider.ChatRequest, emit provider.EmitFunc) (*provider.Result, *store.Account, error) {
	defer r.probe(req)()
	emitted := false
	trackedEmit := func(d provider.Delta) error {
		emitted = true
		return emit(d)
	}
	return r.runAttempts(ctx, req, attemptPlan{
		stream: true,
		invoke: func(acc *store.Account) *provider.Result {
			return r.Provider.StreamChat(ctx, acc, req, trackedEmit)
		},
		emitted: func() bool { return emitted },
	})
}
