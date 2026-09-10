package router

import (
	"context"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// Chat runs a non-streaming completion through the shared retry/failover
// driver. emitted is always false — Chat never streams, so the driver's
// output-started guard is a no-op here.
func (r *Router) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.Result, *store.Account, error) {
	defer r.probe(req)()
	if res := r.cacheGet(req); res != nil {
		return res, nil, nil
	}
	res, acc, err := r.runAttempts(ctx, req, attemptPlan{
		invoke: func(acc *store.Account) *provider.Result {
			return r.Provider.Chat(ctx, acc, req)
		},
		emitted: func() bool { return false },
	})
	if err == nil {
		r.cachePut(req, res)
	}
	return res, acc, err
}
