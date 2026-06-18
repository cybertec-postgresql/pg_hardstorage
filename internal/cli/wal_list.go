// wal_list.go — CLI surface for listing archived WAL segments and repairing slot gaps.
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// newWalListCmd implements `pg_hardstorage wal list <deployment>`.
// Walks the repo's WAL prefix and summarises committed segments —
// timeline distribution, lowest/highest LSN, gap detection.
//
// This is the operator-facing companion to walsink's automated gap-
// detection: an explicit "what WAL do I have?" view that's safe to
// run against any storage backend.
func newWalListCmd() *cobra.Command {
	var (
		repoURL  string
		timeline uint32
		gapsOnly bool
	)
	c := &cobra.Command{
		Use:          "list <deployment>",
		Short:        "List committed WAL segments in the repository",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalList(cmd, args[0], repoURL, timeline, gapsOnly)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().Uint32Var(&timeline, "timeline", 0,
		"only list segments on this timeline (0 = all timelines)")
	c.Flags().BoolVar(&gapsOnly, "gaps-only", false,
		"only print the gap report; suppress per-segment listing")
	return c
}

func runWalList(cmd *cobra.Command, deployment, repoURL string, tliFilter uint32, gapsOnly bool) error {
	d := DispatcherFrom(cmd)
	_, sp, err := repo.Open(cmd.Context(), repoURL)
	if err != nil {
		return mapRepoOpenErr(repoURL, err)
	}
	defer sp.Close()

	segs, err := scanWALSegments(cmd.Context(), sp, deployment, tliFilter)
	if err != nil {
		return err
	}
	body := walListBody{
		Deployment: deployment,
		Timelines:  summariseTimelines(segs),
		GapCount:   countGaps(segs),
	}
	if !gapsOnly {
		body.Segments = segs
	}
	body.Gaps = findGaps(segs)
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// walSegment is one row in the list. We deliberately keep it small;
// a dedicated `wal show <segment>` would parse the manifest body for
// chunk-level detail.
type walSegment struct {
	Timeline      uint32 `json:"timeline"`
	SegmentNumber uint64 `json:"segment_number"`
	SegmentName   string `json:"segment_name"`
	StartLSN      string `json:"start_lsn"`
	EndLSN        string `json:"end_lsn"`
}

// walGap is a contiguous range of missing segments on a single
// timeline.
type walGap struct {
	Timeline     uint32 `json:"timeline"`
	StartSegment uint64 `json:"start_segment"`
	EndSegment   uint64 `json:"end_segment"`
	MissingCount uint64 `json:"missing_count"`
}

// scanWALSegments lists every segment manifest under wal/<dep>/ and
// returns them sorted by (timeline, segment_number).
func scanWALSegments(ctx context.Context, sp storage.StoragePlugin, deployment string, tliFilter uint32) ([]walSegment, error) {
	prefix := "wal/" + deployment + "/"
	const wantSuffix = ".json"
	type rawSeg struct {
		tli  uint32
		base string
		key  string
	}
	var raws []rawSeg
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return nil, output.NewError("wal.list_failed",
				fmt.Sprintf("wal list: %v", err)).Wrap(err)
		}
		key := info.Key
		if !strings.HasSuffix(key, wantSuffix) {
			continue
		}
		if strings.Contains(key, ".json.tmp.") {
			continue
		}
		// Layout: wal/<dep>/<TLI-hex>/<24-char>.json. Guard the
		// slice — short keys (e.g. an accidental wal/<dep>/foo.json)
		// would index out-of-range without this.
		if len(key) < len(wantSuffix)+24 {
			continue
		}
		base := key[len(key)-len(wantSuffix)-24 : len(key)-len(wantSuffix)]
		tli, _, ok := parseSegmentNameForFetch(base)
		if !ok {
			continue
		}
		if tliFilter != 0 && tli != tliFilter {
			continue
		}
		raws = append(raws, rawSeg{tli: tli, base: base, key: key})
	}

	// The segment_number and LSNs a name maps to depend on the cluster's
	// wal_segment_size (PG packs 4 GiB / size segments per log-id).
	// Read it from any one segment's manifest — it is cluster-wide and
	// constant — and compute every entry with it. Falls back to the
	// 16 MiB default when no manifest is readable.
	firstKey := ""
	if len(raws) > 0 {
		firstKey = raws[0].key
	}
	segSize := deploymentSegmentSize(ctx, sp, firstKey)

	out := make([]walSegment, 0, len(raws))
	for _, r := range raws {
		_, segNum, _ := walsink.ParseSegmentName(r.base, segSize)
		startLSN := pglogrepl.LSN(uint64(segNum) * uint64(segSize))
		endLSN := startLSN + pglogrepl.LSN(segSize)
		out = append(out, walSegment{
			Timeline:      r.tli,
			SegmentNumber: segNum,
			SegmentName:   r.base,
			StartLSN:      startLSN.String(),
			EndLSN:        endLSN.String(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Timeline != out[j].Timeline {
			return out[i].Timeline < out[j].Timeline
		}
		return out[i].SegmentNumber < out[j].SegmentNumber
	})
	return out, nil
}

// deploymentSegmentSize reads the wal_segment_size recorded on the
// segment manifest at anyKey. The size is cluster-wide and constant, so
// one manifest is representative. Returns the 16 MiB default when the
// key is empty, unreadable, or records an invalid size.
func deploymentSegmentSize(ctx context.Context, sp storage.StoragePlugin, anyKey string) int64 {
	if anyKey == "" {
		return walsink.DefaultSegmentSize
	}
	rc, err := sp.Get(ctx, anyKey)
	if err != nil {
		return walsink.DefaultSegmentSize
	}
	raw, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		return walsink.DefaultSegmentSize
	}
	m, perr := walsink.ParseSegmentManifest(raw)
	if perr != nil || !walsink.ValidSegmentSize(m.SegmentSize) {
		return walsink.DefaultSegmentSize
	}
	return m.SegmentSize
}

// findGaps reports contiguous missing-segment ranges per timeline.
// The list is sorted by (timeline, start_segment).
func findGaps(segs []walSegment) []walGap {
	if len(segs) < 2 {
		return nil
	}
	var gaps []walGap
	for i := 1; i < len(segs); i++ {
		prev, curr := segs[i-1], segs[i]
		if prev.Timeline != curr.Timeline {
			continue
		}
		if curr.SegmentNumber > prev.SegmentNumber+1 {
			gaps = append(gaps, walGap{
				Timeline:     prev.Timeline,
				StartSegment: prev.SegmentNumber + 1,
				EndSegment:   curr.SegmentNumber - 1,
				MissingCount: curr.SegmentNumber - prev.SegmentNumber - 1,
			})
		}
	}
	return gaps
}

func countGaps(segs []walSegment) int {
	return len(findGaps(segs))
}

// summariseTimelines counts segments per timeline. For the at-a-
// glance "what TLIs do I have?" line the list summary prints first.
type walTimelineSummary struct {
	Timeline       uint32 `json:"timeline"`
	SegmentCount   int    `json:"segment_count"`
	LowestSegment  uint64 `json:"lowest_segment"`
	HighestSegment uint64 `json:"highest_segment"`
}

func summariseTimelines(segs []walSegment) []walTimelineSummary {
	by := map[uint32]*walTimelineSummary{}
	for _, s := range segs {
		ts, ok := by[s.Timeline]
		if !ok {
			by[s.Timeline] = &walTimelineSummary{
				Timeline:       s.Timeline,
				SegmentCount:   1,
				LowestSegment:  s.SegmentNumber,
				HighestSegment: s.SegmentNumber,
			}
			continue
		}
		ts.SegmentCount++
		if s.SegmentNumber < ts.LowestSegment {
			ts.LowestSegment = s.SegmentNumber
		}
		if s.SegmentNumber > ts.HighestSegment {
			ts.HighestSegment = s.SegmentNumber
		}
	}
	out := make([]walTimelineSummary, 0, len(by))
	for _, v := range by {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timeline < out[j].Timeline })
	return out
}

type walListBody struct {
	Deployment string               `json:"deployment"`
	Timelines  []walTimelineSummary `json:"timelines"`
	GapCount   int                  `json:"gap_count"`
	Gaps       []walGap             `json:"gaps,omitempty"`
	Segments   []walSegment         `json:"segments,omitempty"`
}

// WriteText renders the WAL inventory — per-timeline counts plus any gap
// findings — as human-readable text to w.
func (b walListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "WAL inventory for %s\n", b.Deployment)
	if len(b.Timelines) == 0 {
		fmt.Fprintln(bw, "  no segments committed")
		_, err := io.WriteString(w, bw.String())
		return err
	}
	for _, t := range b.Timelines {
		fmt.Fprintf(bw, "  TLI %d: %d segments (#%d..#%d)\n",
			t.Timeline, t.SegmentCount, t.LowestSegment, t.HighestSegment)
	}
	if b.GapCount == 0 {
		fmt.Fprintln(bw, "  ✓ no gaps detected")
	} else {
		fmt.Fprintf(bw, "  ✗ %d gap(s):\n", b.GapCount)
		for _, g := range b.Gaps {
			fmt.Fprintf(bw, "      TLI %d: segments #%d..#%d (%d missing)\n",
				g.Timeline, g.StartSegment, g.EndSegment, g.MissingCount)
		}
	}
	if len(b.Segments) > 0 {
		fmt.Fprintln(bw, "")
		for _, s := range b.Segments {
			fmt.Fprintf(bw, "  %s  TLI %d  %s -> %s\n",
				s.SegmentName, s.Timeline, s.StartLSN, s.EndLSN)
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// newWalRepairCmd implements `pg_hardstorage wal repair <deployment>`.
// Recreates a dropped replication slot and reports the gap (if any)
// between the slot's new restart_lsn and the LSN we last archived.
//
// This is the v0.1 minimum: confirm slot existence, create if absent
// (idempotent), report any gap. Future revisions add explicit
// "rebootstrap from last backup's stop_lsn" mechanics.
func newWalRepairCmd() *cobra.Command {
	var (
		pgConn   string
		repoURL  string
		slotName string
	)
	c := &cobra.Command{
		Use:          "repair <deployment>",
		Short:        "Recreate a missing replication slot; report any LSN gap",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalRepair(cmd, args[0], pgConn, repoURL, slotName)
		},
	}
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string for the source PostgreSQL (required)")
	_ = c.MarkFlagRequired("pg-connection")
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — used to compute the gap from the highest archived segment (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&slotName, "slot", "",
		"replication slot name (default: pg_hardstorage_<deployment>)")
	return c
}

func runWalRepair(cmd *cobra.Command, deployment, pgConn, repoURL, slotName string) error {
	d := DispatcherFrom(cmd)
	if slotName == "" {
		slotName = walStreamAppName(deployment)
	}

	// Probe IDENTIFY_SYSTEM up-front so a bad DSN is caught before
	// we touch the slot.
	idConn, err := pg.Connect(cmd.Context(), pgConn, pg.ModeReplication)
	if err != nil {
		return output.NewError("connect.replication",
			fmt.Sprintf("wal repair: %v", err)).Wrap(err)
	}
	identity, err := pg.IdentifySystem(cmd.Context(), idConn)
	_ = idConn.Close(cmd.Context())
	if err != nil {
		return output.NewError("pg.identify_failed",
			fmt.Sprintf("wal repair: IDENTIFY_SYSTEM: %v", err)).Wrap(err)
	}

	// Compute the highest archived LSN from the repo BEFORE
	// touching the slot. This is what we hand to EnsureSlot as
	// "lastConfirmedLSN": it's the operator's notion of "we have
	// WAL up to here on disk; tell me whether the slot has fallen
	// behind that".
	_, sp, err := repo.Open(cmd.Context(), repoURL)
	if err != nil {
		return mapRepoOpenErr(repoURL, err)
	}
	defer sp.Close()
	// Delegated to the public inventory helper so the leader-
	// follow coordinator and `wal repair` query the same source
	// of truth. Behaviour is byte-equivalent to the v0.1
	// in-package helper.
	highestEnd, _, listErr := inventory.HighestArchivedLSN(cmd.Context(), sp,
		deployment, uint32(identity.Timeline))
	if listErr != nil {
		return output.NewError("repo.list_failed",
			fmt.Sprintf("wal repair: list segments: %v", listErr)).Wrap(listErr)
	}

	// EnsureSlot is the+ Mechanism 2 primitive: probes the
	// slot, recreates with RESERVE_WAL if missing, returns the gap
	// analysis. We open the two connections it needs (regular for
	// pg_replication_slots, replication for CREATE_REPLICATION_SLOT)
	// and close them once we have the result.
	regConn, err := pg.Connect(cmd.Context(), pgConn, pg.ModeRegular)
	if err != nil {
		return output.NewError("connect.regular",
			fmt.Sprintf("wal repair: %v", err)).Wrap(err)
	}
	defer regConn.Close(cmd.Context())
	slotConn, err := pg.Connect(cmd.Context(), pgConn, pg.ModeReplication)
	if err != nil {
		return output.NewError("connect.replication",
			fmt.Sprintf("wal repair: open replication conn: %v", err)).Wrap(err)
	}
	defer slotConn.Close(cmd.Context())

	cont, err := replication.EnsureSlot(cmd.Context(), regConn, slotConn, slotName, highestEnd)
	if err != nil {
		return output.NewError("wal.slot_repair_failed",
			fmt.Sprintf("wal repair: %v", err)).Wrap(err)
	}

	body := walRepairBody{
		Deployment:      deployment,
		Slot:            slotName,
		Timeline:        uint32(identity.Timeline),
		HighestArchived: highestEnd.String(),
		Outcome:         string(cont.Outcome),
		SlotPresent:     true, // EnsureSlot guarantees the slot exists post-call
	}
	if cont.Slot != nil {
		body.SlotRestartLSN = cont.Slot.RestartLSN
		body.SlotActive = cont.Slot.Active
	}

	// Gap interpretation:
	//   - cont.GapBytes > 0 → slot's restart_lsn is AHEAD of the
	//     highest archived LSN (we hand-rolled lastConfirmedLSN as
	//     highestEnd). That means PG advanced past the last byte
	//     we have on disk → archive missed those bytes → real gap.
	//     SlotMinusArchivedBytes is positive (slot ahead of archive).
	//   - cont.GapBytes == 0 → either SlotFound, OR SlotRecreated
	//     where lastConfirmedLSN >= restart_lsn (we're past PG's
	//     position; no gap in the archive's favor). The signed
	//     SlotMinusArchivedBytes preserves the legacy
	//     "slot is N bytes behind archive" diagnostic for the
	//     latter case.
	if cont.Slot != nil && cont.Slot.RestartLSN != "" {
		slotLSN, parseErr := pglogrepl.ParseLSN(cont.Slot.RestartLSN)
		if parseErr == nil {
			body.SlotMinusArchivedBytes = int64(slotLSN) - int64(highestEnd)
			if cont.HasGap() {
				body.GapDetected = true
				body.GapBytes = cont.GapBytes
				body.GapStartLSN = cont.GapStartLSN.String()
				body.GapEndLSN = cont.GapEndLSN.String()
			}
		}
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type walRepairBody struct {
	Deployment      string `json:"deployment"`
	Slot            string `json:"slot"`
	Timeline        uint32 `json:"timeline"`
	HighestArchived string `json:"highest_archived_lsn"`

	// Outcome is the+ Mechanism 2 strategy outcome:
	//   "found"     — slot existed (Strategy A/B), no recreation
	//   "recreated" — slot was missing, recreated with RESERVE_WAL
	// The string value is part of the v1 contract; renaming it
	// would break monitoring scripts that pivot on it.
	Outcome string `json:"outcome,omitempty"`

	// Existing v0.1 fields — kept for back-compat. SlotPresent is
	// now always true post-EnsureSlot (EnsureSlot guarantees the
	// slot exists when it returns nil error).
	SlotPresent            bool   `json:"slot_present"`
	SlotActive             bool   `json:"slot_active"`
	SlotRestartLSN         string `json:"slot_restart_lsn,omitempty"`
	SlotMinusArchivedBytes int64  `json:"slot_minus_archived_bytes"`
	GapDetected            bool   `json:"gap_detected"`

	// New+ fields giving the gap structurally. GapBytes is
	// unsigned (a real gap is always non-negative — it's the slot
	// AHEAD of the archive). GapStartLSN / GapEndLSN are the
	// archive's last-known and the slot's current restart_lsn
	// respectively, in PG LSN string form.
	GapBytes    uint64 `json:"gap_bytes,omitempty"`
	GapStartLSN string `json:"gap_start_lsn,omitempty"`
	GapEndLSN   string `json:"gap_end_lsn,omitempty"`
}

// WriteText renders the WAL repair outcome — slot state and any structural
// gap detail — as human-readable text to w.
func (b walRepairBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "✓ wal repair complete")
	fmt.Fprintf(bw, "  Deployment:        %s\n", b.Deployment)
	fmt.Fprintf(bw, "  Slot:              %s\n", b.Slot)
	if b.Outcome != "" {
		fmt.Fprintf(bw, "  Outcome:           %s\n", b.Outcome)
	}
	fmt.Fprintf(bw, "  Slot present:      %t\n", b.SlotPresent)
	fmt.Fprintf(bw, "  Slot active:       %t\n", b.SlotActive)
	if b.SlotRestartLSN != "" {
		fmt.Fprintf(bw, "  Slot restart_lsn:  %s\n", b.SlotRestartLSN)
	}
	fmt.Fprintf(bw, "  Highest archived:  %s\n", b.HighestArchived)
	if b.GapDetected {
		fmt.Fprintf(bw, "  ✗ GAP detected: slot advanced %d bytes past archive (%s → %s)\n",
			b.GapBytes, b.GapStartLSN, b.GapEndLSN)
	} else {
		fmt.Fprintf(bw, "  ✓ no gap (slot %d bytes %s archive)\n",
			absI64(b.SlotMinusArchivedBytes),
			gapDirection(b.SlotMinusArchivedBytes))
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func absI64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func gapDirection(n int64) string {
	switch {
	case n > 0:
		return "ahead of"
	case n < 0:
		return "behind"
	}
	return "even with"
}
