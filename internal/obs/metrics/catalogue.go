// catalogue.go — the pg_hardstorage metric catalogue, bound to a process
// -wide default registry.  The names, label sets, and types here are the
// runtime realisation of docs/reference/metric-catalogue.md.
//
// Call sites use the typed helpers (BackupStarted, WALSegmentArchived,
// …) rather than touching the registry directly, so the label schema for
// each metric lives in exactly one place and can't drift between emit
// sites.  Everything is registered lazily at package init via the
// package-level Vec values, so a binary that never imports a producer
// still exposes the full schema (HELP/TYPE with zero series) the moment
// its /metrics endpoint is scraped.
package metrics

// All metric names share this prefix, per the catalogue's "Conventions
// (committed)" section.
const namespace = "pg_hardstorage_"

// defaultReg is the process-wide registry the /metrics endpoints render.
var defaultReg = NewRegistry()

// Default returns the process-wide registry.  Wiring code attaches
// scrape-time collectors to it (e.g. the control plane's job/agent gauge
// collector) and serves Default().Handler() at /metrics.
func Default() *Registry { return defaultReg }

// Latency bucket layouts.  Seconds-scale operations (backups, verifies)
// and sub-second operations (KMS unwrap, HTTP requests) get different
// bucket ranges so the histograms keep useful resolution where the mass
// of observations actually falls.
var (
	secondsBuckets = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}
	fastBuckets    = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
)

// --- Process / build ------------------------------------------------------

var buildInfo = defaultReg.RegisterGauge(
	namespace+"build_info",
	"Build metadata as labels; value is always 1.",
	"version", "commit")

// SetBuildInfo records the running binary's version + commit as a
// constant 1-valued gauge.  Call once at startup from any long-running
// process (control plane, agent).
func SetBuildInfo(version, commit string) {
	buildInfo.With(version, commit).Set(1)
}

// --- Backup pipeline ------------------------------------------------------

var (
	backupStarted = defaultReg.RegisterCounter(
		namespace+"backup_started_total",
		"Backups started, by deployment and type.",
		"deployment", "type")

	backupCompleted = defaultReg.RegisterCounter(
		namespace+"backup_completed_total",
		"Backups that reached a terminal state; result is success or failure.",
		"deployment", "type", "result")

	backupDuration = defaultReg.RegisterHistogram(
		namespace+"backup_duration_seconds",
		"Wall-clock duration of completed backups.",
		secondsBuckets, "deployment", "type")

	backupBytesLogical = defaultReg.RegisterGauge(
		namespace+"backup_bytes_logical",
		"Logical (pre-dedup, pre-compress) bytes in the latest backup.",
		"deployment")

	backupBytesPhysical = defaultReg.RegisterGauge(
		namespace+"backup_bytes_physical",
		"On-the-wire bytes the latest backup wrote to the repo.",
		"deployment")

	backupDedupRatio = defaultReg.RegisterGauge(
		namespace+"backup_dedup_ratio",
		"physical/logical byte ratio of the latest backup (lower is better dedup).",
		"deployment")

	chunkUploads = defaultReg.RegisterCounter(
		namespace+"chunk_uploads_total",
		"CAS chunk uploads, by outcome: ok (freshly written) or dedup.",
		"deployment", "result")
)

// BackupStarted increments the started counter.
func BackupStarted(deployment, typ string) { backupStarted.With(deployment, typ).Inc() }

// BackupCompleted increments the completed counter with result
// "success" or "failure".
func BackupCompleted(deployment, typ, result string) {
	backupCompleted.With(deployment, typ, result).Inc()
}

// ObserveBackupDuration records a completed backup's wall-clock seconds.
func ObserveBackupDuration(deployment, typ string, seconds float64) {
	backupDuration.With(deployment, typ).Observe(seconds)
}

// SetBackupBytes records the latest backup's logical + physical byte
// totals and derives the dedup ratio.  physical/logical < 1 means the
// repo stored fewer bytes than the database holds (dedup + compression
// won).
func SetBackupBytes(deployment string, logical, physical int64) {
	backupBytesLogical.With(deployment).Set(float64(logical))
	backupBytesPhysical.With(deployment).Set(float64(physical))
	if logical > 0 {
		backupDedupRatio.With(deployment).Set(float64(physical) / float64(logical))
	}
}

// AddChunkUploads records CAS chunk outcomes for one backup: how many
// chunks were freshly written (result="ok") vs served from dedup
// (result="dedup").  The label values mirror the reserved catalogue
// layout for pg_hardstorage_chunk_uploads_total.
func AddChunkUploads(deployment string, written, deduped int64) {
	if written > 0 {
		chunkUploads.With(deployment, "ok").Add(float64(written))
	}
	if deduped > 0 {
		chunkUploads.With(deployment, "dedup").Add(float64(deduped))
	}
}

// --- WAL pipeline ---------------------------------------------------------

var (
	walSegmentsArchived = defaultReg.RegisterCounter(
		namespace+"wal_segments_archived_total",
		"WAL segments archived to the repo.",
		"deployment")

	walArchiveBytes = defaultReg.RegisterCounter(
		namespace+"wal_archived_bytes_total",
		"Logical bytes of WAL archived to the repo.",
		"deployment")
)

// WALSegmentArchived records one archived WAL segment of the given
// logical size.
func WALSegmentArchived(deployment string, segmentBytes int64) {
	walSegmentsArchived.With(deployment).Inc()
	if segmentBytes > 0 {
		walArchiveBytes.With(deployment).Add(float64(segmentBytes))
	}
}

// --- Verify ---------------------------------------------------------------

var verifyRuns = defaultReg.RegisterCounter(
	namespace+"verify_runs_total",
	"Verify invocations, by deployment and result (success/failure/skipped).",
	"deployment", "result")

// VerifyRun records the outcome of one verify run.
func VerifyRun(deployment, result string) { verifyRuns.With(deployment, result).Inc() }

// --- KMS ------------------------------------------------------------------

var kmsUnwrapLatency = defaultReg.RegisterHistogram(
	namespace+"kms_unwrap_latency_seconds",
	"DEK-unwrap round-trip latency, by KMS scheme.",
	fastBuckets, "scheme", "result")

// ObserveKMSUnwrap records one UnwrapDEK round-trip.
func ObserveKMSUnwrap(scheme, result string, seconds float64) {
	kmsUnwrapLatency.With(scheme, result).Observe(seconds)
}

// --- Control-plane HTTP ---------------------------------------------------

var (
	httpRequests = defaultReg.RegisterCounter(
		namespace+"http_requests_total",
		"Control-plane HTTP requests, by route, method, and status code.",
		"route", "method", "code")

	httpDuration = defaultReg.RegisterHistogram(
		namespace+"http_request_duration_seconds",
		"Control-plane HTTP request handling latency, by route.",
		fastBuckets, "route")
)

// HTTPRequest records one served HTTP request.
func HTTPRequest(route, method, code string, seconds float64) {
	httpRequests.With(route, method, code).Inc()
	httpDuration.With(route).Observe(seconds)
}

// --- Control-plane gauges (scrape-time collectors set these) --------------

var (
	jobsGauge = defaultReg.RegisterGauge(
		namespace+"jobs",
		"Jobs known to the control plane, by state.",
		"state")

	agentsGauge = defaultReg.RegisterGauge(
		namespace+"agents",
		"Agents registered with the control plane, by liveness state.",
		"state")

	reposConfigured = defaultReg.RegisterGauge(
		namespace+"repos_configured",
		"Number of repositories the control plane is configured to serve.")
)

// SetJobsByState publishes the control plane's per-state job counts.
func SetJobsByState(counts map[string]int) {
	for state, n := range counts {
		jobsGauge.With(state).Set(float64(n))
	}
}

// SetAgents publishes the active/total agent counts.
func SetAgents(active, total int) {
	agentsGauge.With("active").Set(float64(active))
	agentsGauge.With("total").Set(float64(total))
}

// SetReposConfigured publishes how many repos the control plane serves.
func SetReposConfigured(n int) { reposConfigured.With().Set(float64(n)) }

// --- Restore --------------------------------------------------------------

var (
	restoreStarted = defaultReg.RegisterCounter(
		namespace+"restore_started_total",
		"Restores started, by deployment.",
		"deployment")

	restoreCompleted = defaultReg.RegisterCounter(
		namespace+"restore_completed_total",
		"Restores that reached a terminal state; result is success or failure.",
		"deployment", "result")

	restoreDuration = defaultReg.RegisterHistogram(
		namespace+"restore_duration_seconds",
		"Wall-clock duration of completed restores.",
		secondsBuckets, "deployment")
)

// RestoreStarted increments the restore-started counter.
func RestoreStarted(deployment string) { restoreStarted.With(deployment).Inc() }

// RestoreCompleted increments the restore-completed counter with result
// "success" or "failure".
func RestoreCompleted(deployment, result string) { restoreCompleted.With(deployment, result).Inc() }

// ObserveRestoreDuration records a completed restore's wall-clock seconds.
func ObserveRestoreDuration(deployment string, seconds float64) {
	restoreDuration.With(deployment).Observe(seconds)
}

// --- Replicate ------------------------------------------------------------

var (
	replicateRuns = defaultReg.RegisterCounter(
		namespace+"replicate_runs_total",
		"Cross-repo replicate runs that reached a terminal state; result is success, incomplete, or failure.",
		"result")

	replicateObjectsCopied = defaultReg.RegisterCounter(
		namespace+"replicate_objects_copied_total",
		"Objects copied to the destination by replicate, by kind (manifest/chunk/wal_manifest).",
		"kind")

	replicateBytesCopied = defaultReg.RegisterCounter(
		namespace+"replicate_bytes_copied_total",
		"Bytes copied to the destination by replicate.")
)

// ReplicateRun records one replicate run's terminal result.
func ReplicateRun(result string) { replicateRuns.With(result).Inc() }

// AddReplicateCopied records the objects + bytes a replicate run copied.
func AddReplicateCopied(manifests, chunks, walManifests int, bytes int64) {
	if manifests > 0 {
		replicateObjectsCopied.With("manifest").Add(float64(manifests))
	}
	if chunks > 0 {
		replicateObjectsCopied.With("chunk").Add(float64(chunks))
	}
	if walManifests > 0 {
		replicateObjectsCopied.With("wal_manifest").Add(float64(walManifests))
	}
	if bytes > 0 {
		replicateBytesCopied.With().Add(float64(bytes))
	}
}

// --- Control-plane agent loop ---------------------------------------------

var controlPlaneErrors = defaultReg.RegisterCounter(
	namespace+"controlplane_errors_total",
	"Agent control-plane loop errors, by op (heartbeat/claim/progress/complete).",
	"op")

// ControlPlaneError records one agent control-plane loop error so a
// fleet whose agents can't reach the control plane is alertable, not just
// visible in stderr.
func ControlPlaneError(op string) { controlPlaneErrors.With(op).Inc() }
