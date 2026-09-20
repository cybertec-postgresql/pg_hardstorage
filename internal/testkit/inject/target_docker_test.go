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
func TestIsCgroupLimitUnreachable(t *testing.T) {
	// The 10 h v1.5.0 soak (seed 20260920) logged this runc
	// refusal once in 152 cgroup_squeeze applications. Typing
	// it as ErrLimitUnreachable depends on matching that text
	// and not matching generic docker-update failures.
	cases := []struct {
		out  string
		want bool
	}{
		{
			out: `Error response from daemon: runc did not terminate successfully:
    failed to write "33554432": write /sys/fs/cgroup/docker/abc/memory.max: invalid argument`,
			want: true,
		},
		{out: "is not running", want: false},
		{out: "Memory limit should be smaller than already set memoryswap limit", want: false},
		{out: "failed to write foo to /sys/fs/cgroup/memory.max", want: true},
		{out: "permission denied", want: false},
	}
	for _, tc := range cases {
		got := isCgroupLimitUnreachable(tc.out)
		if got != tc.want {
			t.Errorf("isCgroupLimitUnreachable(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

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
