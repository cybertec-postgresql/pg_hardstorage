// wal_stream_timeline.go — choosing WHICH timeline to open the stream on.
//
// resolveStartLSN answers "from where do we resume". This answers the
// other half of the same question — "on which timeline does that LSN
// live" — which for a long time was not asked at all: the stream was
// always opened on the timeline IDENTIFY_SYSTEM reported.
//
// The two answers can disagree, and when they do the request is for a
// segment file that has never existed. See internal/wal/timeline's
// history.go for the full account; the short version is that a
// streamer resuming from an older timeline's frontier asked the NEW
// timeline's walsender for it, got "requested WAL segment ... has
// already been removed", and stopped permanently — with the WAL still
// on disk under the old timeline's name.
package cli

import (
	"context"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

// historyReader is the slice of *timeline.Store this needs, so the
// decision can be tested without a repository behind it.
type historyReader interface {
	Get(ctx context.Context, deployment string, tli uint32) ([]byte, error)
}

// resolveStreamTimeline returns the timeline to name in
// START_REPLICATION for a stream resuming at startLSN, given the
// timeline the server currently reports.
//
// Almost always that IS the server's timeline: a cluster that has
// never failed over has no other, and a streamer that kept up resumes
// inside the live one. It differs only when the resume point predates
// the current timeline's fork — the case resolveStartLSN's
// walk-back arm creates — and there the old timeline is the only one
// that can serve those bytes. PostgreSQL streams it forward and ends
// the COPY at the switchpoint (surfacing as ErrServerClosedStream),
// the retry loop reconnects, and this resolves one timeline further
// along; the chain is walked one promotion per reconnect until the
// stream reaches the live timeline and stays there.
//
// Degraded paths all fall back to serverTLI, which is exactly what the
// code did before this existed: no history file (timeline 1, or a
// capture that failed) and an unparseable one both leave PostgreSQL to
// answer for itself rather than have us guess an ancestor.
func resolveStreamTimeline(
	ctx context.Context,
	store historyReader,
	deployment string,
	serverTLI uint32,
	startLSN pglogrepl.LSN,
	segSize int64,
	emit func(*output.Event),
) uint32 {
	if store == nil || serverTLI <= 1 {
		return serverTLI
	}
	warn := func(reason string) {
		if emit == nil {
			return
		}
		emit(output.NewEvent(output.SeverityWarning, "wal.timeline", "history_unreadable").
			WithSubject(output.Subject{Deployment: deployment, Timeline: serverTLI}).
			WithBody(map[string]any{
				"error": reason,
				"message": "could not read this timeline's history, so the stream opens on the " +
					"timeline the server reports. If the resume point predates this timeline's " +
					"fork, PostgreSQL will refuse it as an already-removed segment even though " +
					"the WAL is still held under the previous timeline.",
			}))
	}

	content, err := store.Get(ctx, deployment, serverTLI)
	if err != nil {
		warn(err.Error())
		return serverTLI
	}
	hist, err := timeline.ParseHistory(content)
	if err != nil {
		warn(err.Error())
		return serverTLI
	}
	chosen := timeline.Containing(hist, serverTLI, startLSN, segSize)
	if chosen != serverTLI && emit != nil {
		emit(output.NewEvent(output.SeverityInfo, "wal.timeline", "streaming_historic").
			WithSubject(output.Subject{Deployment: deployment, Timeline: chosen}).
			WithBody(map[string]any{
				"server_timeline": serverTLI,
				"start_lsn":       startLSN.String(),
				"message": "the resume point predates the current timeline's fork, so the stream " +
					"opens on the timeline that actually holds it. PostgreSQL serves it forward " +
					"and ends the stream at the branch point; the reconnect picks up on the next " +
					"timeline, one promotion at a time, until the stream reaches the live one.",
			}))
	}
	return chosen
}

// wrap a *timeline.Store (or nil) as a historyReader without handing a
// typed nil to the interface.
func historyStore(sp storage.StoragePlugin) historyReader {
	if sp == nil {
		return nil
	}
	return timeline.New(sp)
}
