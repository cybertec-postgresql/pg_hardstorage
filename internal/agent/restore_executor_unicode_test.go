package agent

import (
	"testing"
	"time"
)

// TestBuildRecoveryFromArgs_UnicodeToName pins that a non-ASCII
// control-plane restore-point name (the JSON "to_name" field a remote
// operator POSTs) is carried into Recovery.TargetName verbatim. The
// shared render path (restore.WriteRecoveryFiles) then quotes it into
// recovery_target_name; this test guards the agent's args→Recovery
// translation specifically, where a JSON-decoded UTF-8 string must not
// be mangled or dropped.
func TestBuildRecoveryFromArgs_UnicodeToName(t *testing.T) {
	names := []string{
		"نقطة-الاستعادة",       // Arabic (RTL)
		"точка-восстановления", // Cyrillic
		"恢复点-生产",               // CJK
		"復元ポイント",               // Japanese
		"restore-✅-🚀",          // emoji (4-byte)
		"o'brien's-點",          // embedded quote + CJK
	}
	for _, name := range names {
		// args["to_name"] is what a decoded JSON body yields.
		r, err := buildRecoveryFromArgs(map[string]any{"to_name": name}, time.Time{})
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if r == nil {
			t.Errorf("%q: nil Recovery", name)
			continue
		}
		if r.TargetName != name {
			t.Errorf("TargetName mismatch:\n  want %q\n  got  %q", name, r.TargetName)
		}
		if r.TargetLSN != "" || !r.TargetTime.IsZero() {
			t.Errorf("%q: only TargetName should be set; got lsn=%q time=%v",
				name, r.TargetLSN, r.TargetTime)
		}
	}
}
