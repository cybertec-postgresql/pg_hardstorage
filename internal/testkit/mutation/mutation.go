// Package mutation implements the testkit's mutation-testing
// harness.  Closes the SPEC commitment for "mutation testing.
// Build-tagged fault injection in our own code (e.g.,
// `//go:build mutation_chunker_off_by_one`) — re-run scenarios;
// assertions must catch the mutation.  If they don't, we have a
// coverage gap."
//
// Pattern: each target function is split into two implementations
// guarded by mutually-exclusive build tags:
//
//   - <name>.go               with `//go:build !mutation_<tag>`
//     contains the real implementation
//   - <name>_mutation_<tag>.go with `//go:build mutation_<tag>`
//     contains a deliberately-broken variant
//
// The harness runs `go test -tags=mutation_<tag>` against the
// affected packages and asserts the test suite FAILS — i.e. our
// existing tests catch the regression.  A mutation that doesn't
// trigger any failure is a coverage gap and surfaces as a hard
// failure in this harness's own run.
//
// To add a new mutation:
//  1. Pick a target function whose correctness matters.
//  2. Move it to its own file with `!mutation_<tag>` constraint.
//  3. Add a sibling `<name>_mutation_<tag>.go` with the mutated
//     version under `mutation_<tag>`.
//  4. Append a Mutation entry to Registry below naming the tag,
//     the package(s) where breakage is expected, and a brief
//     description.
//  5. Run `go test -tags=mutation_runner ./internal/testkit/mutation`
//     to confirm the mutation is caught.
package mutation

// Mutation describes one mutation-testing entry.  The harness
// loops over Registry and runs `go test -tags=<Tag>` against each
// package in Packages, expecting the test invocation to fail.
type Mutation struct {
	// Tag is the build tag the mutated source file is guarded by
	// (e.g. "mutation_chunkkey_no_suffix").  The harness passes
	// this through `go test -tags=...`.
	Tag string

	// Description is a one-line operator-readable note explaining
	// what the mutation breaks and which test class catches it.
	Description string

	// Packages is the set of import paths the harness runs `go
	// test` against under the mutation tag.  Each package's test
	// suite must fail for the mutation to be considered "caught".
	// Listing one package per mutation keeps wallclock reasonable;
	// the harness exits as soon as it sees a non-zero exit from
	// any of them.
	Packages []string
}

// Registry is the canonical list of mutations the harness runs.
// Adding an entry requires a corresponding pair of source files
// in the named package — see the package doc.
var Registry = []Mutation{
	{
		Tag: "mutation_materialize_offset_unchecked",
		Description: "materializeFile writes restored chunks in slice " +
			"order without confirming each chunk's Offset matches the " +
			"running write position (pre-fix): a reordered or gapped " +
			"chunk list that still sums to the file size restores " +
			"BYTE-SCRAMBLED data that passes the size check with no " +
			"error — silent corruption on the primary restore path. " +
			"Manifest.Validate guards it upstream, but the reassembly " +
			"must not rest the byte-order of restored data on a single " +
			"guard. Caught by TestMaterializeFile_ReorderedChunksDetected.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore",
		},
	},
	{
		Tag: "mutation_partial_offset_unchecked",
		Description: "materialiseOneFile (partial table extraction) writes " +
			"chunks in slice order without confirming each chunk's Offset " +
			"matches the running write position (pre-fix): a reordered chunk " +
			"list that still sums to the file size hands back a BYTE-SCRAMBLED " +
			"table file that passes the size check with no error. The partial " +
			"path was a copy of the pre-#32 materializeFile and missed the " +
			"guard. Caught by TestMaterialiseOneFile_RejectsReorderedChunks.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/partial",
		},
	},
	{
		Tag: "mutation_gap_slot_key_dropped",
		Description: "the gap-record storage key omits its per-slot token " +
			"(pre-fix): a single Patroni failover reconciles every " +
			"configured slot in the same instant, so two slots that gap on " +
			"the same timeline at the same unix-nano share a key; the second " +
			"slot's IfNotExists PUT is rejected as already-present and its " +
			"gap record is SILENTLY DROPPED — restore then can't refuse a " +
			"PITR into that lost gap. Caught by " +
			"TestPut_TwoSlotsSameTimelineSameInstantBothSurvive.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate",
		},
	},
	{
		Tag: "mutation_gap_purge_grace_dropped",
		Description: "`wal gap-purge --orphans` treats every tombstoned " +
			"timeline as dead, ignoring the GC grace (pre-fix): a " +
			"within-grace tombstoned backup — still restorable via `backup " +
			"undelete` — drops out of the live set, so a gap on an " +
			"intermediate timeline its recovery_target_timeline='latest' " +
			"PITR would cross gets reaped, and restore silently loses the " +
			"refusal that flags the missing WAL. gc / wal-prune honour this " +
			"grace; gap-purge must match. Caught by " +
			"TestWalGapPurge_Orphans_KeepsGapForWithinGraceTombstone.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_agent_rotation_abort_on_livedesc",
		Description: "the agent's scheduled rotation skips ONLY a held " +
			"manifest, treating a parent kept alive by its held child " +
			"(ErrChainHasLiveDescendants) as run-fatal (pre-fix): one hold " +
			"then aborts every nightly rotation and any deletable backup " +
			"ordered after the held anchor is never reclaimed — unbounded " +
			"repo/WAL growth on the unattended path nobody watches. The " +
			"interactive `rotate` path protects held chains' ancestors " +
			"up-front; the agent must skip both reactively. Caught by " +
			"TestAgentRotate_HoldOnIncrementalDoesNotWedgeScheduledRetention.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_dek_retention_dropped",
		Description: "the shared DEK — the single object that decrypts EVERY " +
			"encrypted chunk in the repo — is written without a WORM " +
			"RetainUntil (pre-fix): on a compliance repo the chunks, " +
			"manifests, WAL and timeline history are all Object-Locked but " +
			"the DEK is deletable, so deleting one tiny object loses ALL " +
			"encrypted data while the immutable chunks remain unreadable. " +
			"The worst WORM gap there is. Caught by " +
			"TestResolveOrMint_WORMLocksAndExtendsSharedDEK.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/sharedkey",
		},
	},
	{
		Tag: "mutation_twelvehour_unchecked",
		Description: "the natural-time PITR parser accepts malformed " +
			"12-hour clocks (pre-fix): '13am' resolves to 13:00 (1pm), " +
			"'0am'/'0pm' to midnight/noon, so an operator meaning 1am who " +
			"typos 13am silently gets a recovery_target_time twelve hours " +
			"off — a confidently-wrong PITR target no downstream layer can " +
			"catch. Caught by " +
			"TestParse_MalformedTwelveHourClockRejected.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime",
		},
	},
	{
		Tag: "mutation_wal_removed_retried",
		Description: "the stream never recognises PostgreSQL's 'requested " +
			"WAL segment has already been removed' verdict (the " +
			"evidence-based replacement for the retired predictive " +
			"start-vs-restart_lsn refusal, bug #28): recycled WAL is " +
			"retried forever — issue #45's endless-loop shape — and the " +
			"operator's re-anchor guidance never surfaces. Caught by " +
			"TestDecideStreamStop_WalRemovedIsEvidenceBasedTerminal.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_fetch_sysid_unchecked",
		Description: "wal fetch hands foreign WAL to recovery without an " +
			"identity check (pre-#26): an archive mixing cluster lineages " +
			"(reused deployment name after a wipe, a pre-guard mix, " +
			"--allow-system-identifier-change) reaches PostgreSQL, which " +
			"notices only MID-REPLAY with a FATAL naming neither the " +
			"deployment nor the repair. With the seed's identifier " +
			"threaded through restore_command, the first foreign segment " +
			"is refused typed — and the strict tail aborts recovery " +
			"loudly at that byte. Caught by " +
			"TestCheckFetchSystemIdentifier_MismatchRefuses.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_sftp_no_reconnect",
		Description: "the SFTP plugin never reconnects after a keepalive " +
			"teardown (pre-fix bug #25): the keepalive rightly turns a dead " +
			"peer into an error instead of a hang, but the HANDLE stayed " +
			"dead — a days-long `wal stream` holds one storage plugin and " +
			"the CLI opens the repo once outside its retry loop, so one " +
			"70-second network stall stopped archiving until an operator " +
			"restarted the process. Caught by " +
			"TestReconnect_AfterTeardown_OpsRecover (blackhole cleared, " +
			"next operation must heal through a fresh connection).",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp",
		},
	},
	{
		Tag: "mutation_sftp_no_keepalive",
		Description: "the SFTP plugin has no connection keepalive " +
			"(pre-fix bug #23, caught live by the storage fault soak: a " +
			"worker sat 14 minutes inside pkg/sftp sendPacket): " +
			"ssh.ClientConfig.Timeout covers only the dial, so a silently " +
			"dead peer (NAT expiry, power-off, partition) blocks every " +
			"operation forever — a wal fetch inside restore_command hangs " +
			"recovery indefinitely, a backup never finishes and never " +
			"fails, an archiver stalls while pg_wal fills. Caught by " +
			"TestKeepalive_DeadConnectionErrorsInsteadOfHanging (in-proc " +
			"SSH server behind a blackhole proxy; the connection must be " +
			"torn down and the parked Get must error within the miss " +
			"budget).",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/sftp",
		},
	},
	{
		Tag: "mutation_history_preflight_absent",
		Description: "restore has no timeline-history reachability " +
			"preflight (pre-fix bug #22): PostgreSQL probes <N>.history " +
			"ascending and stops at the FIRST miss, so one unarchived or " +
			"lost history file makes a --to-latest recovery silently end " +
			"on an older timeline and promote — success reported, every " +
			"newer-timeline segment in the archive ignored. A pinned " +
			"--timeline N fails the same way without N.history. Caught by " +
			"TestPreflightTimelineHistory_MissingHistory_Refuses, " +
			"_IntermediateHoleRefuses, and _PinnedTimeline.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore",
		},
	},
	{
		Tag: "mutation_restore_cmd_lenient_tail",
		Description: "the one-shot restore_command tail passes " +
			"infrastructure-fault exit codes through instead of dying by " +
			"signal (pre-fix bug #21): PostgreSQL treats EVERY plain " +
			"nonzero restore_command exit as 'file not available' — " +
			"end-of-archive during unbounded recovery — so an S3 outage, " +
			"an expired credential, a keyring refusal, a corrupted " +
			"manifest, a gc-swept chunk, or a missing agent binary " +
			"(exit 127) all ended recovery cleanly and PROMOTED silently " +
			"behind. Only a signal-death aborts recovery. Caught by " +
			"TestBuild_ExitCodeMappingThroughRealShell (death-by-SIGABRT " +
			"assertions through a real shell) and " +
			"TestBuild_ContainsExitCodeMapping.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd",
		},
	},
	{
		Tag: "mutation_prestream_gap_ignores_frontier",
		Description: "the fresh-slot gap recorder never consults the " +
			"archive frontier (pre-fix bug #20): after a Patroni failover " +
			"destroys the slot, the reconnect records [oldest-backup.stop, " +
			"new-anchor) — claiming months of successfully archived WAL as " +
			"a gap — and because gap records are eternal, every unbounded " +
			"restore from every backup older than the failover refuses " +
			"forever, training operators to --skip-gap-check past the " +
			"refusals that are true. Caught by " +
			"TestRecordPreStreamGap_FrontierBoundsTheWindow, " +
			"_FrontierOnPriorTimeline, and _FrontierCoversStart_NoRecord.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_undelete_wal_unchecked",
		Description: "backup undelete verifies chunks but never the WAL " +
			"(pre-fix bug #19): a tombstoned backup does not hold the " +
			"prune frontier, so `wal prune` legitimately deletes the " +
			"archived window after its stop — resurrection then hands " +
			"back a backup whose --to-latest PROMOTES silently behind at " +
			"the pruned hole (a standby freezes forever), with no gap " +
			"record for any preflight to refuse on. Caught by " +
			"TestRecordResurrectedWALGap_PrunedWindow_Records, " +
			"_Idempotent, and the command-level wiring test " +
			"TestBackupUndelete_RecordsPrunedWALGap.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_timetarget_blanket_refusal",
		Description: "the time/name-target gap preflight refuses on ANY " +
			"recorded gap, ignoring whether the seed backup's replay can " +
			"reach it (pre-fix bug #18): gapstate records are eternal, so " +
			"once retention expires the pre-gap generation, every " +
			"`--to <time>` restore of the deployment refuses forever — a " +
			"permanent false positive that trains operators to pass " +
			"--skip-gap-check, disabling the true refusals too. Caught by " +
			"TestPreflightTimeTargetGap_GapBelowSeedStop_Allowed, " +
			"_ManifestGapBelowStop_Allowed, and " +
			"TestEmitTimeTargetGapWarning_BoundedBySeedStop.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore",
		},
	},
	{
		Tag: "mutation_stream_first_record_unchecked",
		Description: "walsink accepts a reconnect's OPENING record at any " +
			"LSN (pre-20afaf5): a stream that resumed past a hole looks " +
			"contiguous forever after, and PG recycles the missing WAL. " +
			"Caught by TestSink_OpeningRecordPastTheResumePoint_Refused.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink",
		},
	},
	{
		Tag: "mutation_adoption_unrecorded",
		Description: "the CAS forgets which chunks it ADOPTED rather than " +
			"wrote (pre-c31688b), so the commit-time dedup-vs-GC gates " +
			"have nothing to re-Stat: a backup or WAL segment commits a " +
			"manifest over chunks gc swept mid-flight, born unrestorable " +
			"while reporting success. Caught in BOTH consumers: " +
			"backup/runner's verifyAdoptedChunks tests and walsink's " +
			"AdoptedChunkSweptMidStream test.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner",
			"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink",
		},
	},
	{
		Tag: "mutation_standby_source_unguarded",
		Description: "wal stream never asks pg_is_in_recovery (pre-6fc79f8): " +
			"after a failover a single-host DSN reconnects to the DEMOTED " +
			"node and silently archives second-hand WAL from a replica — " +
			"measured at 90s of healthy-looking streaming before the guard " +
			"existed. Caught by " +
			"TestGuardSourceIsPrimary_UnreachableFailsOpenButWarns, which " +
			"demands the probe-failure warning the mutant never emits.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_undelete_argv_order",
		Description: "batch undelete processes IDs in argv order " +
			"(pre-3af06d4): cascade_deleted is LEAF-first and the store " +
			"refuses an incremental under a tombstoned ancestor, so the " +
			"documented unwind — pass the slice straight back — fails on " +
			"its first ID. Caught by " +
			"TestCascadeUnwind_RoundTripRestoresTheChain.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_undelete_no_postflip_check",
		Description: "undelete skips the chunk re-verification at the " +
			"VISIBILITY point (pre-983dc4e): the pre-flight ran while the " +
			"manifest was hidden and gc's sweep uses an older snapshot, so " +
			"an undelete racing the sweep returns restored=true for a " +
			"backup whose chunks are gone — a phantom --check-chunks " +
			"vouched for. Caught by " +
			"TestUndelete_SweptDuringUndelete_RetombstonesAndRefuses.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/backup",
		},
	},
	{
		Tag: "mutation_gap_timeline_skip",
		Description: "cli.findGaps regains the pre-59816d8 `continue` on a " +
			"timeline change, making `wal audit` — and the chaos soak's " +
			"gap-free gate, which runs it — structurally blind to a WAL " +
			"hole straddling a failover, the one place an HA deployment " +
			"is most likely to lose WAL. Caught by " +
			"TestFindGaps_AcrossATimelineChange and " +
			"TestWalAudit_HoleStraddlingAPromotionIsDetected. This is the " +
			"exact code that shipped until 2026-08-07, alongside a test " +
			"whose data asserted the blindness was correct.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_frontier_no_prior_timeline",
		Description: "inventory.HighestArchivedLSNBefore never finds a " +
			"prior timeline — the pre-c2c9aa4 world where nothing looked " +
			"below the current one. A post-promotion `wal stream` resume " +
			"then anchors at the new leader's position, silently skipping " +
			"every byte since the old timeline's frontier, and the " +
			"agent's coordinator reads the post-promotion frontier as a " +
			"first-time bootstrap, suppressing the gap calculation on the " +
			"one event it exists to measure. Caught by " +
			"TestResolveStartLSN_AfterPromotion* and " +
			"TestArchiveFrontierForLeader_*.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/cli",
		},
	},
	{
		Tag: "mutation_exit_route_undocumented",
		Description: "output.codePrefixToExit gains a namespace route " +
			"(quarantine.*) and a leaf route (storage.no_space) that " +
			"docs/reference/exit-codes.md does not list. " +
			"TestExitCodes_NoUndocumentedRoutes must catch both. This " +
			"mutation exists because the previous version of that test " +
			"iterated hand-written route lists and could NOT catch a new " +
			"case, while its comment claimed it did — a namespace could " +
			"ship with an exit code no operator could look up.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/output",
		},
	},
	{
		Tag: "mutation_chunkkey_no_suffix",
		Description: "repo.ChunkKey drops the .chk suffix; round-trip + " +
			"ParseChunkKey tests must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/repo",
		},
	},
	{
		Tag: "mutation_audit_hash_zeroed",
		Description: "audit.ComputeHash returns a constant zero hash; " +
			"chain-walking + Append-genesis tests must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/audit",
		},
	},
	{
		Tag: "mutation_threshold_off_by_one",
		Description: "threshold.quorumMet uses > instead of >=; " +
			"members==threshold no longer passes; QuorumMet test must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold",
		},
	},
	{
		Tag: "mutation_lsn_shape_loose",
		Description: "restore.LooksLikeLSN drops the 'exactly one slash' " +
			"check, re-introducing the silent multi-slash regression " +
			"(0//0 sneaks through).  Property tests in " +
			"recovery_property_test.go must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore",
		},
	},
	{
		Tag: "mutation_target_reachable_off_by_one",
		Description: "restore.targetReachable drops the exclusive-stop " +
			"strictness and uses >= for both modes, re-introducing the " +
			"issue-#99 bug where `--to-lsn <stop> --to-exclusive` is " +
			"silently accepted (recovery would run to end-of-WAL).  The " +
			"exclusive-equality case in plan_reachability_test.go must " +
			"catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/restore",
		},
	},
	{
		Tag: "mutation_identifier_no_length_cap",
		Description: "pg.ValidIdentifier drops the 1..63 byte length cap, " +
			"accepting arbitrarily-long (and empty) identifiers.  Property " +
			"tests in identifier_property_test.go must catch it.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/pg",
		},
	},
	{
		Tag: "mutation_stale_temp_fullkey",
		Description: "isStaleTempKey matches the `.json.tmp.` / `.history.tmp.` " +
			"staging marker anywhere in the FULL key instead of the basename " +
			"(pre-fix). Three sites route through it: FindStaleTempManifests " +
			"(a COMMITTED manifest under a dotted deployment/backup ID — " +
			"validateStorageID permits dots, e.g. `.../evil.json.tmp.x/" +
			"manifest.json` — is DELETED by `repo gc --apply`) and " +
			"CollectReferences' WAL+logical walks (a committed segment under a " +
			"deployment named `db.json.tmp.x` is dropped from the ref set, so " +
			"gc reaps its live chunks). Both are silent data loss. Caught by " +
			"TestFindStaleTemp_NeverFlagsLiveManifest + " +
			"TestCollectReferences_HarvestsSegmentUnderDottedDeployment.",
		Packages: []string{
			"github.com/cybertec-postgresql/pg_hardstorage/internal/repo",
		},
	},
}
