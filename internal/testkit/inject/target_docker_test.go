package inject

import "testing"

// TestDockerMemoryLimitArg_UnlimitedUsesSentinel pins the fix for the
// silent cgroup_squeeze recovery failure: SetMemoryLimit(-1) used to
// pass `--memory=-1` to `docker update`, which modern Docker CLI
// (29.x verified) rejects with `invalid size: '-1'`.  The recovery
// path swallowed that error, so the container stayed clamped at the
// squeeze limit and the next backup inside the cell was OOM-killed
// (exit 137).  We now translate `bytes <= 0` to a 1 PiB sentinel
// that Docker accepts.
func TestDockerMemoryLimitArg_UnlimitedUsesSentinel(t *testing.T) {
	const sentinel = "1125899906842624" // 1 PiB

	for _, in := range []int64{-1, 0, -1 << 30} {
		got := dockerMemoryLimitArg(in)
		if got != sentinel {
			t.Errorf("dockerMemoryLimitArg(%d) = %q, want %q (unlimited sentinel)",
				in, got, sentinel)
		}
	}
}

// TestDockerMemoryLimitArg_PositiveBytesPassThrough confirms the
// squeeze path still emits the caller's exact byte count.
func TestDockerMemoryLimitArg_PositiveBytesPassThrough(t *testing.T) {
	cases := map[int64]string{
		1:        "1",
		33554432: "33554432", // the 32 MiB squeeze value used in soak
		1 << 40:  "1099511627776",
	}
	for in, want := range cases {
		got := dockerMemoryLimitArg(in)
		if got != want {
			t.Errorf("dockerMemoryLimitArg(%d) = %q, want %q", in, got, want)
		}
	}
}
