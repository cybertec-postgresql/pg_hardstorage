// llm_audit.go — buildAuditEmitter: wires a chat AuditEmitter through a repo's hash-chained audit store.
package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/chat"
)

// buildAuditEmitter opens the named repo and returns a chat
// AuditEmitter wired through its hash-chained audit Store.
// Best-effort: a failure here is surfaced to the caller as an
// error, but chat still works without audit (the emitter is
// optional on chat.Session).
//
// Closes nothing — the storage plugin's lifetime extends
// beyond the chat session. + might track and close at
// session end; for the leak is noise on the order of one
// open file handle per chat session.
func buildAuditEmitter(ctx context.Context, repoURL string) (chat.AuditEmitter, error) {
	if repoURL == "" {
		return nil, nil
	}
	repoMeta, sp, err := openRepo(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("open audit repo: %w", err)
	}
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	return newAuditChainEmitter(ctx, store, repoURL, "", ""), nil
}

// auditChainEmitter wraps an audit.Store so chat.Session events
// flow into the hash-chained Merkle audit log.  Every prompt,
// tool call, tool result, response and error gets one
// audit.Event whose Action is one of the llm.* verbs the
// session emits.
//
// Best-effort: we route through Store.AppendOrLog so a failing
// audit write doesn't strand the chat session.  The package-
// level fallback logger surfaces the failure to stderr.
type auditChainEmitter struct {
	ctx    context.Context
	store  *audit.Store
	repo   string
	actor  string
	tenant string
}

func newAuditChainEmitter(ctx context.Context, store *audit.Store, repo, actor, tenant string) *auditChainEmitter {
	return &auditChainEmitter{
		ctx:    ctx,
		store:  store,
		repo:   repo,
		actor:  actor,
		tenant: tenant,
	}
}

// Emit implements chat.AuditEmitter.  Constructs an audit.Event
// from the chat AuditEvent and appends it to the chain via
// AppendOrLog (failures route through the package-level
// fallback logger; the chat session continues either way).
func (e *auditChainEmitter) Emit(ev chat.AuditEvent) {
	if e == nil || e.store == nil {
		return
	}
	body := map[string]any{
		"session_id": ev.SessionID,
		"skill":      ev.Skill,
		"provider":   ev.Provider,
	}
	for k, v := range ev.Body {
		body[k] = v
	}
	e.store.AppendOrLog(e.ctx, &audit.Event{
		Action:    ev.Action,
		Actor:     e.actor,
		Tenant:    e.tenant,
		Timestamp: time.Now().UTC(),
		Subject: audit.Subject{
			Repo:   e.repo,
			Tenant: e.tenant,
		},
		Body: body,
	})
}
