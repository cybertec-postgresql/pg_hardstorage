// logical.go — CLI surface for logical-replication stream registration and streaming.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// newRealLogicalCmd implements `pg_hardstorage logical <add|list|remove|status|stream>`.
//
// What ships in v0.1:
//
//   - Stream registry (add / list / remove / status) backed by a JSON
//     state file at paths.State()/logical_streams.json.
//   - `logical stream <name>` long-running consumer that creates the
//     PG-side logical slot (idempotent), opens the replication-mode
//     connection, and pushes XLogData payloads into the chunked sink.
//   - pgoutput as the sole output plugin.
//
// What's deliberately deferred:
//
//   - `logical status` lag computation against PG's
//     pg_replication_slots view (needs a separate regular-mode
//     connection; we report the registry side only in v0.1).
//   - Kafka / Pub/Sub / webhook sinks..
//   - Source-side publication creation. The operator must have
//     created the publication on the source PG; v0.1 verifies it
//     exists when `logical stream` runs by failing-fast on
//     pgoutput's "publication does not exist" error.
func newRealLogicalCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "logical <add|list|remove|status|stream>",
		Short: "Configure logical decoding sinks alongside physical WAL streaming",
		Long: `Manage per-deployment logical decoding pipelines.

A pipeline is the tuple (deployment, stream-name, slot, plugin,
publication, sink). Logical decoding is an OPTIONAL second stream
that complements the physical WAL stream — it does NOT replace it.
A deployment with only logical configured is still NOT considered
"backed up" — physical is the truth-of-record.

v0.1 ships pgoutput + chunked sink. The chunked sink writes batched
XLogData payloads into the same CAS-backed repo as physical WAL,
under logical/<deployment>/<stream-name>/<start-lsn>.json.`,
	}
	c.AddCommand(
		newLogicalAddCmd(),
		newLogicalListCmd(),
		newLogicalRemoveCmd(),
		newLogicalStatusCmd(),
		newLogicalStreamCmd(),
	)
	return c
}

// --- add --------------------------------------------------------------

func newLogicalAddCmd() *cobra.Command {
	var (
		deployment  string
		repoURL     string
		slot        string
		plugin      string
		publication string
		sinkKind    string
	)
	c := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a logical-decoding stream",
		Long: `Append a stream to the registry. Does NOT create the slot or the
publication on the source PG; both must already exist (publication)
or will be created lazily by 'logical stream' (slot).`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			// v0.1 surface guards: only one plugin and one sink kind
			// are wired up. Reject typos here so the operator sees a
			// clear usage error instead of a silently-unsupported
			// stream that fails at the first 'logical stream' run.
			switch plugin {
			case "", "pgoutput":
			default:
				return output.NewError("usage.bad_plugin",
					fmt.Sprintf("logical add: --plugin %q: only \"pgoutput\" is supported in v0.1", plugin)).Wrap(output.ErrUsage)
			}
			switch sinkKind {
			case "", "chunked":
			default:
				return output.NewError("usage.bad_sink",
					fmt.Sprintf("logical add: --sink %q: only \"chunked\" is supported in v0.1", sinkKind)).Wrap(output.ErrUsage)
			}
			// PG replication-slot and publication names follow the
			// PG identifier rule: start with letter/underscore, then
			// alnum/underscore, max 63 chars. Catching it here means
			// 'CREATE_REPLICATION_SLOT %s ...' on the server side
			// receives a well-formed identifier and can't be mis-
			// parsed by the replication-protocol tokenizer.
			if slot != "" && !pg.ValidIdentifier(slot) {
				return output.NewError("usage.bad_slot",
					fmt.Sprintf("logical add: --slot %q: must be a PG identifier "+
						"(start with a letter or underscore, then [a-z0-9_], ≤63 chars)",
						slot)).Wrap(output.ErrUsage)
			}
			if !pg.ValidIdentifier(publication) {
				return output.NewError("usage.bad_publication",
					fmt.Sprintf("logical add: --publication %q: must be a PG identifier "+
						"(start with a letter or underscore, then [a-z0-9_], ≤63 chars)",
						publication)).Wrap(output.ErrUsage)
			}
			mgr, err := logicalManager()
			if err != nil {
				return err
			}
			s, err := mgr.Add(logical.AddOptions{
				Name:        args[0],
				Deployment:  deployment,
				Slot:        slot,
				Plugin:      plugin,
				Publication: publication,
				SinkKind:    sinkKind,
				RepoURL:     repoURL,
			})
			if err != nil {
				return mapLogicalError("logical add", err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(logicalAddBody{Stream: s}))
		},
	}
	c.Flags().StringVar(&deployment, "deployment", "", "source deployment (required)")
	_ = c.MarkFlagRequired("deployment")
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&slot, "slot", "", "logical slot name (default: pg_hardstorage_logical_<name>)")
	c.Flags().StringVar(&plugin, "plugin", "pgoutput", "output plugin (v0.1: pgoutput)")
	c.Flags().StringVar(&publication, "publication", "", "publication name on source PG (required)")
	_ = c.MarkFlagRequired("publication")
	c.Flags().StringVar(&sinkKind, "sink", "chunked", "sink kind (v0.1: chunked)")
	return c
}

// --- list -------------------------------------------------------------

func newLogicalListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List registered logical streams",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			mgr, err := logicalManager()
			if err != nil {
				return err
			}
			out, err := mgr.List()
			if err != nil {
				return output.NewError("logical.list_failed",
					fmt.Sprintf("logical list: %v", err)).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(logicalListBody{Streams: out}))
		},
	}
}

// --- remove -----------------------------------------------------------

func newLogicalRemoveCmd() *cobra.Command {
	var dropSlot bool
	var pgConn string
	c := &cobra.Command{
		Use:          "remove <name>",
		Short:        "Remove a registered logical stream",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			mgr, err := logicalManager()
			if err != nil {
				return err
			}
			s, err := mgr.Get(args[0])
			if err != nil {
				return mapLogicalError("logical remove", err)
			}
			if dropSlot {
				// Flag-gated: --pg-connection needed only when dropping the slot.
				if err := requireFlags(cmd, "pg-connection"); err != nil {
					return err
				}
				if err := dropPGSlot(cmd, pgConn, s.Slot); err != nil {
					return err
				}
			}
			if err := mgr.Remove(args[0]); err != nil {
				return mapLogicalError("logical remove", err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(logicalRemoveBody{
				Name:        args[0],
				DroppedSlot: dropSlot,
			}))
		},
	}
	c.Flags().BoolVar(&dropSlot, "drop-slot", false,
		"also drop the PG-side replication slot (requires --pg-connection)")
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq DSN for the source PG (only needed with --drop-slot)")
	return c
}

func dropPGSlot(cmd *cobra.Command, dsn, slotName string) error {
	c, err := pg.Connect(cmd.Context(), dsn, pg.ModeReplication)
	if err != nil {
		return output.NewError("connect.replication",
			fmt.Sprintf("logical remove: open replication conn: %v", err)).Wrap(err)
	}
	defer c.Close(cmd.Context())
	if err := logicalreceiver.DropLogicalSlot(cmd.Context(), c, slotName); err != nil {
		return output.NewError("logical.drop_slot_failed",
			fmt.Sprintf("logical remove: %v", err)).Wrap(err)
	}
	return nil
}

// --- status -----------------------------------------------------------

func newLogicalStatusCmd() *cobra.Command {
	var pgConn string
	c := &cobra.Command{
		Use:   "status [<name>]",
		Short: "Report registry state for one or all streams",
		Long: `status walks the registry and (when --pg-connection is set)
queries pg_replication_slots to report the current lag.

Without <name> + --pg-connection, the output is the registry view
only — fast, no PG round-trip.

With --pg-connection <url>, status reports for the named stream
also include the slot's restart_lsn / confirmed_flush_lsn /
active flag, and the byte distance behind pg_current_wal_lsn().
Use this to answer "is my logical pipeline keeping up?" without
having to log into the source DB.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogicalStatus(cmd, args, pgConn)
		},
	}
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string (regular mode) — when set, surfaces lag info from pg_replication_slots")
	return c
}

func runLogicalStatus(cmd *cobra.Command, args []string, pgConn string) error {
	d := DispatcherFrom(cmd)
	mgr, err := logicalManager()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		// No specific stream — emit the registry list. --pg-connection
		// is ignored at the list level (probing every slot would be
		// O(streams) round-trips; operators who want fleet lag use the
		// control plane's status endpoint).
		out, err := mgr.List()
		if err != nil {
			return mapLogicalError("logical status", err)
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(logicalListBody{Streams: out}))
	}
	s, err := mgr.Get(args[0])
	if err != nil {
		return mapLogicalError("logical status", err)
	}
	body := logicalStatusBody{Stream: *s}
	if pgConn != "" {
		lag, lerr := logical.Lag(cmd.Context(), pgConn, s.Slot)
		switch {
		case lerr == nil:
			body.Lag = lag
		case errors.Is(lerr, logical.ErrSlotNotFound):
			body.LagError = "slot not present in pg_replication_slots — has the stream ever connected?"
		default:
			body.LagError = lerr.Error()
		}
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// --- stream -----------------------------------------------------------

func newLogicalStreamCmd() *cobra.Command {
	var (
		pgConn            string
		startLSN          string
		statusInterval    time.Duration
		inactivityTimeout time.Duration
	)
	c := &cobra.Command{
		Use:   "stream <name>",
		Short: "Run the long-lived consumer for a registered stream",
		Long: `Open a logical-replication connection, ensure the slot exists,
and consume into the configured sink (v0.1: chunked).

Each batch of XLogData is buffered up to the chunked sink's
BatchBytes (default 16 MiB) or BatchInterval (default 5s) — whichever
fires first — and committed atomically. Standby Status Updates
forward the sink's SyncedLSN every status-interval, advancing the
slot's confirmed_flush_lsn so PG can release retained WAL.

Send SIGINT or SIGTERM to stop cleanly. Any in-flight batch is
flushed before exit.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogicalStream(cmd, logicalStreamOptions{
				name:              args[0],
				pgConn:            pgConn,
				startLSN:          startLSN,
				statusInterval:    statusInterval,
				inactivityTimeout: inactivityTimeout,
			})
		},
	}
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string for the source PG (required)")
	c.Flags().StringVar(&startLSN, "start-lsn", "",
		"explicit start LSN (default: 0/0 — let the slot drive)")
	c.Flags().DurationVar(&statusInterval, "status-interval", 10*time.Second,
		"status-update cadence")
	c.Flags().DurationVar(&inactivityTimeout, "inactivity-timeout", 0,
		"abort if no message arrives in this duration (0 = streaming default)")
	return c
}

type logicalStreamOptions struct {
	name              string
	pgConn            string
	startLSN          string
	statusInterval    time.Duration
	inactivityTimeout time.Duration
}

func runLogicalStream(cmd *cobra.Command, opts logicalStreamOptions) error {
	d := DispatcherFrom(cmd)
	if err := requireFlags(cmd, "pg-connection"); err != nil {
		return err
	}
	mgr, err := logicalManager()
	if err != nil {
		return err
	}
	stream, err := mgr.Get(opts.name)
	if err != nil {
		return mapLogicalError("logical stream", err)
	}

	_, sp, err := repo.Open(cmd.Context(), stream.RepoURL)
	if err != nil {
		return mapRepoOpenErr(stream.RepoURL, err)
	}
	defer sp.Close()
	if err := assertRepoWritable(cmd.Context(), sp, "logical stream"); err != nil {
		return err
	}
	cas := casdefault.New(sp)

	// 1. Ensure the slot exists. Idempotent.
	{
		c, err := pg.Connect(cmd.Context(), opts.pgConn, pg.ModeReplication)
		if err != nil {
			return output.NewError("connect.replication",
				fmt.Sprintf("logical stream: %v", err)).Wrap(err)
		}
		if err := logicalreceiver.CreateLogicalSlot(cmd.Context(), c, stream.Slot, stream.Plugin); err != nil {
			c.Close(cmd.Context())
			return output.NewError("logical.create_slot_failed",
				fmt.Sprintf("logical stream: %v", err)).
				WithSuggestion(&output.Suggestion{
					Human: "the replication user needs the REPLICATION attribute, and pg_hba.conf must permit `replication` from this host. The publication " + stream.Publication + " must already exist on the source PG.",
				}).Wrap(err)
		}
		c.Close(cmd.Context())
	}

	// 2. Build the sink.
	sink, err := chunked.New(cas, sp, chunked.Options{
		Deployment: stream.Deployment,
		StreamName: stream.Name,
		Slot:       stream.Slot,
		Plugin:     stream.Plugin,
	})
	if err != nil {
		return output.NewError("logical.sink_init_failed",
			fmt.Sprintf("logical stream: %v", err)).Wrap(err)
	}

	// 3. Resolve start LSN.
	var startLSN pglogrepl.LSN
	if opts.startLSN != "" {
		lsn, err := pglogrepl.ParseLSN(opts.startLSN)
		if err != nil {
			return output.NewError("usage.bad_lsn",
				fmt.Sprintf("logical stream: --start-lsn %q: %v", opts.startLSN, err)).Wrap(output.ErrUsage)
		}
		startLSN = lsn
	}

	// 4. Stream.
	streamConn, err := pg.Connect(cmd.Context(), opts.pgConn, pg.ModeReplication)
	if err != nil {
		return output.NewError("connect.replication",
			fmt.Sprintf("logical stream: %v", err)).Wrap(err)
	}

	// Install a signal handler that cancels on SIGINT/SIGTERM.
	streamCtx, cancel := installLogicalSignalCancel(cmd.Context())
	defer cancel()

	// pgoutput plugin args: proto_version=2 is broadly supported (PG
	// 14+); operators on PG 13 can override via env. The
	// publication_names argument is required.
	args := []string{
		"proto_version '2'",
		fmt.Sprintf("publication_names '%s'", stream.Publication),
	}

	startedAt := time.Now().UTC()
	streamErr := logicalreceiver.Stream(streamCtx, streamConn, logicalreceiver.StreamOptions{
		Slot:                 stream.Slot,
		StartLSN:             startLSN,
		PluginArgs:           args,
		StatusUpdateInterval: opts.statusInterval,
		InactivityTimeout:    opts.inactivityTimeout,
	}, sink)
	stoppedAt := time.Now().UTC()

	// Final flush so any partial batch durably commits before we
	// return.
	if err := sink.Flush(cmd.Context()); err != nil {
		return output.NewError("logical.final_flush_failed",
			fmt.Sprintf("logical stream: final flush: %v", err)).Wrap(err)
	}

	body := logicalStreamResultBody{
		Name:       stream.Name,
		Deployment: stream.Deployment,
		Slot:       stream.Slot,
		StartedAt:  startedAt,
		StoppedAt:  stoppedAt,
		DurationMS: stoppedAt.Sub(startedAt).Milliseconds(),
		SyncedLSN:  sink.SyncedLSN().String(),
		CleanStop:  errors.Is(streamErr, context.Canceled),
	}
	if streamErr != nil && !isCleanStop(streamErr) {
		return output.NewError("logical.stream_error",
			fmt.Sprintf("logical stream: %v", streamErr)).Wrap(streamErr)
	}
	body.CleanStop = true
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// --- helpers ---------------------------------------------------------

func logicalManager() (*logical.Manager, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, output.NewError("paths.resolve_failed",
			fmt.Sprintf("logical: resolve paths: %v", err)).Wrap(err)
	}
	state := filepath.Join(p.State.String(), "logical_streams.json")
	return logical.NewManager(state), nil
}

func mapLogicalError(op string, err error) error {
	switch {
	case errors.Is(err, logical.ErrAlreadyExists):
		return output.NewError("conflict.logical_exists",
			fmt.Sprintf("%s: %v", op, err)).Wrap(err)
	case errors.Is(err, logical.ErrNotFound):
		return output.NewError("notfound.logical",
			fmt.Sprintf("%s: %v", op, err)).Wrap(err)
	}
	return output.NewError("logical.failed",
		fmt.Sprintf("%s: %v", op, err)).Wrap(err)
}

// installLogicalSignalCancel mirrors the wal-stream pattern: cancels
// streamCtx on SIGINT/SIGTERM so Ctrl-C ends the consumer cleanly.
func installLogicalSignalCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sig)
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// isCleanStop reports whether err is "user-initiated cancellation" vs
// "real failure". Mirrors the wal-stream classification.
func isCleanStop(err error) bool {
	return errors.Is(err, context.Canceled)
}

// --- bodies -----------------------------------------------------------

type logicalAddBody struct {
	Stream *logical.Stream `json:"stream"`
}

// WriteText renders the registered stream's metadata as human-readable text to w.
func (b logicalAddBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ logical stream %s\n", b.Stream.Name)
	fmt.Fprintf(bw, "  Deployment:  %s\n", b.Stream.Deployment)
	fmt.Fprintf(bw, "  Slot:        %s\n", b.Stream.Slot)
	fmt.Fprintf(bw, "  Plugin:      %s\n", b.Stream.Plugin)
	fmt.Fprintf(bw, "  Publication: %s\n", b.Stream.Publication)
	fmt.Fprintf(bw, "  Sink:        %s → %s", b.Stream.SinkKind, b.Stream.RepoURL)
	_, err := io.WriteString(w, bw.String())
	return err
}

// logicalStatusBody is the body for `logical status <name>`. Lag is
// populated only when the operator passed --pg-connection; it stays
// nil for the registry-only path. LagError carries the structured
// reason when a lag probe failed (e.g. slot not yet created).
type logicalStatusBody struct {
	Stream   logical.Stream     `json:"stream"`
	Lag      *logical.LagResult `json:"lag,omitempty"`
	LagError string             `json:"lag_error,omitempty"`
}

// WriteText renders the stream's registry metadata plus any sampled lag
// figures as human-readable text to w.
func (b logicalStatusBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "logical stream %s\n", b.Stream.Name)
	fmt.Fprintf(bw, "  Deployment:  %s\n", b.Stream.Deployment)
	fmt.Fprintf(bw, "  Slot:        %s\n", b.Stream.Slot)
	fmt.Fprintf(bw, "  Plugin:      %s\n", b.Stream.Plugin)
	fmt.Fprintf(bw, "  Publication: %s\n", b.Stream.Publication)
	fmt.Fprintf(bw, "  Sink:        %s → %s\n", b.Stream.SinkKind, b.Stream.RepoURL)
	if b.Lag != nil {
		active := "idle"
		if b.Lag.Active {
			active = "active"
		}
		fmt.Fprintf(bw, "  Slot:        %s (%s)\n", active, b.Lag.Plugin)
		if b.Lag.RestartLSN != "" {
			fmt.Fprintf(bw, "  restart_lsn: %s\n", b.Lag.RestartLSN)
		}
		if b.Lag.ConfirmedFlushLSN != "" {
			fmt.Fprintf(bw, "  flushed:     %s\n", b.Lag.ConfirmedFlushLSN)
		}
		if b.Lag.CurrentWALLSN != "" {
			fmt.Fprintf(bw, "  primary:     %s\n", b.Lag.CurrentWALLSN)
		}
		fmt.Fprintf(bw, "  behind:      %s\n", humanBytes(b.Lag.BehindBytes))
	} else if b.LagError != "" {
		fmt.Fprintf(bw, "  Lag:         %s\n", b.LagError)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type logicalListBody struct {
	Streams []logical.Stream `json:"streams"`
}

// WriteText renders the registered logical streams as a tabular summary to w.
func (b logicalListBody) WriteText(w io.Writer) error {
	if len(b.Streams) == 0 {
		_, err := io.WriteString(w, "no logical streams configured")
		return err
	}
	bw := &strings.Builder{}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDEPLOYMENT\tSLOT\tPUBLICATION\tSINK\tCREATED")
	for _, s := range b.Streams {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Deployment, s.Slot, s.Publication, s.SinkKind,
			s.CreatedAt.Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type logicalRemoveBody struct {
	Name        string `json:"name"`
	DroppedSlot bool   `json:"dropped_slot"`
}

// WriteText renders the remove confirmation, noting whether the upstream PG
// slot was dropped, as a single-line summary to w.
func (b logicalRemoveBody) WriteText(w io.Writer) error {
	state := "(slot kept on PG)"
	if b.DroppedSlot {
		state = "(slot dropped on PG)"
	}
	_, err := fmt.Fprintf(w, "✓ logical stream %s removed %s", b.Name, state)
	return err
}

type logicalStreamResultBody struct {
	Name       string    `json:"name"`
	Deployment string    `json:"deployment"`
	Slot       string    `json:"slot"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`
	SyncedLSN  string    `json:"synced_lsn"`
	CleanStop  bool      `json:"clean_stop"`
}

// WriteText renders the streaming session summary — synced LSN, duration,
// clean-stop verdict — as human-readable text to w.
func (b logicalStreamResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verb := "✓"
	if !b.CleanStop {
		verb = "✗"
	}
	fmt.Fprintf(bw, "%s logical stream %s\n", verb, b.Name)
	fmt.Fprintf(bw, "  Deployment:  %s\n", b.Deployment)
	fmt.Fprintf(bw, "  Slot:        %s\n", b.Slot)
	fmt.Fprintf(bw, "  Synced LSN:  %s\n", b.SyncedLSN)
	fmt.Fprintf(bw, "  Duration:    %d ms", b.DurationMS)
	_, err := io.WriteString(w, bw.String())
	return err
}
