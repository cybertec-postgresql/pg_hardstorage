// validate.go — `validate` subcommand: soak driver that runs load, injects faults, and verifies backups.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/compose"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

func newValidateCmd() *cobra.Command {
	var (
		fleetPath        string
		profilesPath     string
		faultsPath       string
		duration         time.Duration
		seed             int64
		project          string
		reportDir        string
		dryRun           bool
		faultRate        float64
		healWindow       time.Duration
		backupEvery      int
		verifyEvery      int
		iterInterval     time.Duration
		hostPortBase     int
		dockerBin        string
		profileName      string
		pushgatewayURL   string
		pushInterval     time.Duration
		setupConcurrency int
	)
	c := &cobra.Command{
		Use:   "validate",
		Short: "Soak driver: drives load, injects faults, verifies backups",
		Long: `Runs the soak loop for --duration against the cells in
--fleet, applying faults from --faults probabilistically.
Writes a JSON + Markdown report to --report-dir on completion.

This subcommand expects the docker-compose stack to already be
running (use 'compose generate' + 'docker compose up -d' first).
Pass --dry-run to exercise the orchestrator against a fake
runtime — useful for verifying configs without touching the
fleet.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fleet, err := config.LoadFleet(fleetPath)
			if err != nil {
				return err
			}
			if len(fleet.Entries) == 0 {
				return fmt.Errorf("validate: fleet %s is empty", fleetPath)
			}

			var faults *config.Faults
			if faultsPath != "" {
				faults, err = config.LoadFaults(faultsPath)
				if err != nil {
					return err
				}
				if err := faults.Validate(); err != nil {
					return err
				}
			} else {
				faults = &config.Faults{}
			}
			// Resolve the load profile.  --profile <name>
			// picks one entry from --profiles.  Empty profile
			// pool → default tpcc-lite shape per cell.
			profile, err := resolveProfile(profilesPath, profileName)
			if err != nil {
				return err
			}

			if reportDir == "" {
				reportDir = filepath.Join("test-runs",
					"run-"+time.Now().UTC().Format("20060102T150405Z"))
			}
			if err := os.MkdirAll(reportDir, 0o755); err != nil {
				return err
			}

			cells, err := buildCellRuntimes(fleet, dryRun, hostPortBase,
				dockerBin, project, profile, faults, seed)
			if err != nil {
				return err
			}

			// Pushgateway emitter — empty URL disables.
			pg := validate.NewPushgatewayEmitter(pushgatewayURL,
				"pg_hardstorage_validate",
				fmt.Sprintf("%s-%d", project, seed))
			pg.Interval = pushInterval
			pgCtx, pgCancel := context.WithCancel(cmd.Context())
			defer pgCancel()
			go pg.Run(pgCtx)

			// Persist every event to <report-dir>/events.ndjson so
			// the live-view watcher (`pg_hardstorage_testkit watch
			// <report-dir>`) and post-mortem analysis have a stable
			// source of truth even if stdout is being piped to
			// /dev/null in CI or to a process that crashes.
			eventsPath := filepath.Join(reportDir, "events.ndjson")
			eventsFile, err := os.Create(eventsPath)
			if err != nil {
				return fmt.Errorf("validate: open events.ndjson: %w", err)
			}
			defer eventsFile.Close()
			emitFile := makeNDJSONEmitter(eventsFile)

			// OnEvent fans out to NDJSON stdout + pushgateway +
			// events.ndjson.  Each leg is independent: a slow
			// pushgateway can't stall the file write, and a stdout
			// closed mid-run (operator hit Ctrl-C on the tee) can't
			// corrupt the on-disk record.
			emitJSON := makeNDJSONEmitter(cmd.OutOrStdout())
			emitFn := func(ev validate.Event) {
				emitJSON(ev)
				emitFile(ev)
				pg.OnEvent(ev)
			}
			// Annotate every cell with its OS / PG so the
			// pushed Prometheus metrics carry useful labels.
			for _, e := range fleet.Entries {
				pg.AnnotateCellMetadata(e.Name, e.OS, e.PG)
			}

			rep, err := validate.Run(cmd.Context(), validate.RunOptions{
				Project:  project,
				Seed:     seed,
				Duration: duration,
				Loop: validate.LoopOptions{
					IterationInterval: iterInterval,
					BackupEvery:       backupEvery,
					VerifyEvery:       verifyEvery,
					FaultProbability:  faultRate,
					HealWindow:        healWindow,
				},
				Faults:           faults,
				Cells:            cells,
				OnEvent:          emitFn,
				SetupConcurrency: setupConcurrency,
			})
			if err != nil {
				return err
			}

			// Stamp fleet summary into the report.
			rep.FleetSummary.TotalCells = len(fleet.Entries)
			for _, e := range fleet.Entries {
				rep.FleetSummary.TotalContainers += e.EffectiveContainerCount()
				rep.FleetSummary.OSDistribution[e.OS]++
				rep.FleetSummary.PGDistribution[e.PG]++
			}

			// Populate per-cell metadata from the fleet — the
			// orchestrator only knows cell names; the
			// (OS, PG, arch, role) annotation comes from
			// the source-of-truth fleet entry.
			byName := map[string]config.FleetEntry{}
			for _, e := range fleet.Entries {
				byName[e.Name] = e
			}
			for i := range rep.Cells {
				if e, ok := byName[rep.Cells[i].Name]; ok {
					rep.Cells[i].OS = e.OS
					rep.Cells[i].PG = e.PG
					rep.Cells[i].Arch = e.EffectiveArch()
					rep.Cells[i].Role = e.EffectiveRole()
				}
			}

			// Write report.json + report.md.
			jsonPath := filepath.Join(reportDir, "report.json")
			mdPath := filepath.Join(reportDir, "report.md")
			if err := writeReport(jsonPath, mdPath, rep); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"\nWrote report:\n  %s\n  %s\n", jsonPath, mdPath)
			if !rep.OverallPass {
				return fmt.Errorf("soak run failed: %d cell-failure(s)", len(rep.Failures))
			}
			return nil
		},
	}
	c.Flags().StringVar(&fleetPath, "fleet", defaultFleetPath, "fleet YAML input")
	c.Flags().StringVar(&profilesPath, "profiles", "", "profiles YAML input (optional, type-checked only)")
	c.Flags().StringVar(&faultsPath, "faults", "", "faults YAML input (optional)")
	c.Flags().DurationVar(&duration, "duration", time.Hour, "total soak wall-clock")
	c.Flags().Int64Var(&seed, "seed", time.Now().UnixNano(), "drive-loop rng seed")
	c.Flags().StringVar(&project, "project", "pgvalidate", "docker-compose project name")
	c.Flags().StringVar(&reportDir, "report-dir", "", "report output dir (default: ./test-runs/run-<ts>)")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"use FakeCellRuntime instead of touching real containers")
	c.Flags().Float64Var(&faultRate, "fault-rate", 0.2,
		"per-iteration probability of fault injection (0..1)")
	// 60s (was 30s) because under concurrent host load — multiple
	// soak / compat / k8s slots running simultaneously on one
	// daemon — PG occasionally needs 30–60 s to recover from a
	// SIGINT (replay WAL + reopen network listeners while the
	// host's IO is saturated).  At 30s the inject step records a
	// false-positive "PG did not become ready" failure that
	// reproduced in soak testing's debian-12-pg15 migration cell.
	// 60 s leaves enough margin for the realistic worst case
	// without making no-load-recovery tests visibly slower.
	c.Flags().DurationVar(&healWindow, "heal-window", 60*time.Second,
		"wait between fault apply and recovery")
	c.Flags().IntVar(&backupEvery, "backup-every", 5,
		"take a backup every N iterations")
	c.Flags().IntVar(&verifyEvery, "verify-every", 25,
		"restore-verify every N iterations")
	c.Flags().DurationVar(&iterInterval, "iter-interval", 10*time.Second,
		"sleep between iterations")
	c.Flags().IntVar(&hostPortBase, "host-port-base", 15432,
		"first host port allocated to PG containers (must match `compose generate`)")
	c.Flags().StringVar(&dockerBin, "docker-bin", "docker",
		"docker / podman binary on PATH")
	c.Flags().StringVar(&profileName, "profile", "",
		"profile name from --profiles (default: tpcc-lite shape)")
	c.Flags().StringVar(&pushgatewayURL, "pushgateway", "",
		"Prometheus Pushgateway URL (empty disables; e.g. http://localhost:9091)")
	c.Flags().DurationVar(&pushInterval, "push-interval", 30*time.Second,
		"how often to PUT metrics to the pushgateway")
	c.Flags().IntVar(&setupConcurrency, "setup-concurrency", 0,
		"max cells running Setup() (initdb + waitForPG) at once. "+
			"0 = use the orchestrator default (8). Negative = disable throttling. "+
			"Lower this on tight hosts to avoid initdb storms; raise it on big boxes.")
	return c
}

// buildCellRuntimes constructs per-entry CellRuntime instances.
// In dry-run mode every cell becomes a FakeCellRuntime; in
// production mode each cell becomes a DockerCellRuntime
// targeting the host-mapped PG port allocated by `compose
// generate`.  The two paths live behind the same CellRuntime
// interface so the orchestrator doesn't care which it gets.
func buildCellRuntimes(
	fleet *config.Fleet, dryRun bool, hostPortBase int, dockerBin, project string,
	profile config.Profile, faults *config.Faults, seed int64,
) ([]validate.CellRuntime, error) {
	if dryRun {
		out := make([]validate.CellRuntime, 0, len(fleet.Entries))
		for _, e := range fleet.Entries {
			out = append(out, &validate.FakeCellRuntime{NameStr: e.Name})
		}
		return out, nil
	}
	ports := compose.AllocatePorts(fleet, hostPortBase)
	var out []validate.CellRuntime
	for i, e := range fleet.Entries {
		port, err := compose.PortFor(ports, e)
		if err != nil {
			return nil, err
		}
		container := compose.FirstContainer(e)
		// Per-cell seed: derive from the soak seed + cell
		// index so each cell takes a deterministic load
		// trajectory.
		cellSeed := seed ^ int64(i+1)*1099511628211
		r, err := validate.NewDockerCellRuntime(e, project, container, port,
			profile, faults, cellSeed)
		if err != nil {
			return nil, err
		}
		if dockerBin != "" {
			r.DockerBin = dockerBin
		}
		out = append(out, r)
	}
	return out, nil
}

// resolveProfile returns the workload profile for the soak.
// If profilesPath is empty OR profileName is empty, returns
// the default {Schema: tpcc-lite, ChurnMBPerMin: 50}.
// Otherwise, looks up by name and returns that entry.
func resolveProfile(profilesPath, profileName string) (config.Profile, error) {
	def := config.Profile{
		Name:          "default",
		TargetSizeGB:  10,
		Schema:        "tpcc-lite",
		ChurnMBPerMin: 50,
	}
	if profilesPath == "" {
		return def, nil
	}
	p, err := config.LoadProfiles(profilesPath)
	if err != nil {
		return def, err
	}
	if err := p.Validate(); err != nil {
		return def, err
	}
	if profileName == "" {
		// File supplied but no name picked — use the first
		// profile if any, else default.
		if len(p.Profiles) > 0 {
			return p.Profiles[0], nil
		}
		return def, nil
	}
	if hit := p.FindProfile(profileName); hit != nil {
		return *hit, nil
	}
	return def, fmt.Errorf("validate: profile %q not in %s", profileName, profilesPath)
}

// makeNDJSONEmitter returns an OnEvent func that writes one
// JSON object per line to w.  Soak runs pipe stdout through
// `jq` for live observability.
func makeNDJSONEmitter(w io.Writer) func(validate.Event) {
	enc := json.NewEncoder(w)
	return func(ev validate.Event) {
		_ = enc.Encode(ev)
	}
}

// writeReport drops report.json + report.md to the given paths.
// Both files are mode 0644: the report itself is metadata —
// no credentials inside, deliberately readable so CI's
// archive-artefact step can pick it up without permissions
// fussing.
func writeReport(jsonPath, mdPath string, rep *report.Report) error {
	jf, err := os.OpenFile(jsonPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := rep.WriteJSON(jf); err != nil {
		jf.Close()
		return err
	}
	if err := jf.Close(); err != nil {
		return err
	}
	mf, err := os.OpenFile(mdPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := rep.WriteMarkdown(mf); err != nil {
		mf.Close()
		return err
	}
	return mf.Close()
}
