// CPU + heap profiling hooks for the native CLI.
//
// Wired as persistent flags on the root command (root.go's
// --cpu-profile / --mem-profile / --profile-port) so any
// long-running subcommand can be profiled without a separate
// build.  Off by default; zero overhead when the flags are
// unset.
//
// Usage:
//
//	# CPU profile of a 5-minute wal stream:
//	pg_hardstorage --cpu-profile=/tmp/cpu.pprof wal stream prod-db \
//	    --pg-connection ... --repo file:///srv/repo
//	# (Ctrl-C after enough wall time.  Run-time stop hook
//	# writes the profile.)
//	go tool pprof -http=:8080 /tmp/cpu.pprof
//
//	# Heap profile (snapshot at exit):
//	pg_hardstorage --mem-profile=/tmp/heap.pprof backup prod-db ...
//
//	# Live HTTP pprof endpoint (for long-running agents):
//	pg_hardstorage --profile-port=6060 agent ...
//	go tool pprof -http=:8080 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
//
// startProfiling / stopProfiling are called by Run before
// and after root.ExecuteC.  Errors at start are surfaced
// loudly (cmdline says "profile to /tmp/x" but the file
// can't be created — the operator wants to know now, not
// after a 5-minute run produces nothing).  Errors at stop
// are best-effort: we already ran; losing the profile is
// disappointing but not a reason to mask the underlying
// command's exit code.

package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"runtime"
	"runtime/pprof"

	"github.com/spf13/cobra"
)

// profileHandle holds the open files + http listener so
// stopProfiling can clean up after the command runs.
type profileHandle struct {
	cpuFile *os.File
	memPath string
	httpLis net.Listener
}

// startProfiling reads the persistent flags off the root
// command, initialises whatever was requested, and returns
// a handle for the corresponding stopProfiling call.  Empty
// flags → empty handle → stopProfiling is a no-op.
//
// The cobra.Command we receive is the ROOT (Run is called
// before any subcommand resolves), so the persistent flag
// values are accessible directly via cmd.Flags().
func startProfiling(cmd *cobra.Command) (*profileHandle, error) {
	h := &profileHandle{}

	cpuPath, _ := cmd.Flags().GetString("cpu-profile")
	if cpuPath != "" {
		f, err := os.Create(cpuPath)
		if err != nil {
			return nil, fmt.Errorf("--cpu-profile %q: %w", cpuPath, err)
		}
		// runtime/pprof.StartCPUProfile is safe to call
		// once per process; calling twice (e.g. tests
		// re-invoking Run) errors with "cpu profiling
		// already in use".  We accept the panic-class
		// failure and surface it.
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			_ = os.Remove(cpuPath)
			return nil, fmt.Errorf("start CPU profile: %w", err)
		}
		h.cpuFile = f
	}

	memPath, _ := cmd.Flags().GetString("mem-profile")
	if memPath != "" {
		// Only validate we can create the file at start;
		// the heap snapshot itself happens at stop time
		// so the most-recent allocations are captured.
		f, err := os.Create(memPath)
		if err != nil {
			return nil, fmt.Errorf("--mem-profile %q: %w", memPath, err)
		}
		_ = f.Close()
		h.memPath = memPath
	}

	port, _ := cmd.Flags().GetInt("profile-port")
	if port > 0 {
		// Bind explicitly to loopback so we don't expose
		// pprof to the network — the operator must already
		// be on the box to reach it.
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			// Cleanup the CPU profile we may already
			// have started, then bubble the error.
			if h.cpuFile != nil {
				pprof.StopCPUProfile()
				_ = h.cpuFile.Close()
			}
			return nil, fmt.Errorf("--profile-port %d: %w", port, err)
		}
		// http.DefaultServeMux already has /debug/pprof/*
		// registered via the net/http/pprof import side
		// effect.  Serve in a goroutine; the listener
		// closes at stopProfiling time.
		srv := &http.Server{Handler: http.DefaultServeMux}
		go func() { _ = srv.Serve(lis) }()
		h.httpLis = lis
		fmt.Fprintf(os.Stderr, "pg_hardstorage: pprof endpoint live at http://%s/debug/pprof/\n", addr)
	}

	return h, nil
}

// stopProfiling flushes whatever startProfiling started and
// releases the handles.  Best-effort: errors are written to
// stderr but never alter the command's exit code (the
// command's own result is what the operator cares about).
func stopProfiling(h *profileHandle) {
	if h == nil {
		return
	}
	if h.cpuFile != nil {
		pprof.StopCPUProfile()
		if err := h.cpuFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "pg_hardstorage: close cpu-profile: %v\n", err)
		}
	}
	if h.memPath != "" {
		// Force a GC so the heap profile reflects current
		// reachable allocations rather than stale-but-
		// not-yet-collected ones.  This makes the snapshot
		// match what an operator would see in a `top -i`
		// during steady state.
		runtime.GC()
		f, err := os.Create(h.memPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pg_hardstorage: open mem-profile: %v\n", err)
		} else {
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(os.Stderr, "pg_hardstorage: write mem-profile: %v\n", err)
			}
			_ = f.Close()
		}
	}
	if h.httpLis != nil {
		_ = h.httpLis.Close()
	}
}

// Compile-time guards against unused imports if the build
// trims something.
var _ = context.Canceled
