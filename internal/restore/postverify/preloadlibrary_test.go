package postverify

import "testing"

// postverify boots the restored cluster to prove it comes up. When it
// does not come up, the reason decides whether the operator has a
// broken backup or a mis-provisioned host — and those two answers send
// them to completely different places at the worst possible moment.
//
// A backup taken from a Patroni/Spilo cluster carries
// `shared_preload_libraries = bg_mon,...` in its postgresql.conf.
// Restored onto stock PostgreSQL, the postmaster dies with
//
//	FATAL:  could not access file "bg_mon": No such file or directory
//
// before it has looked at a single data page. The restore was
// perfect. Reporting restore.postverify_failed there tells someone
// mid-disaster-recovery that their backup did not survive, which is
// false and is the most expensive wrong answer this tool can give.
//
// The softening has to stay narrow, though: every failure that IS
// about the data must stay loud, because catching those is the entire
// reason postverify exists.

func TestMissingPreloadLibrary_HostProblems(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "spilo bg_mon, the case that surfaced this",
			log: `2026-09-22 15:29:10 UTC [4023144]: [1-1] 0 FATAL:  could not access file "bg_mon": No such file or directory
2026-09-22 15:29:10 UTC [4023144]: [2-1] 0 LOG:  database system is shut down`,
			want: "bg_mon",
		},
		{
			name: "a contrib module the host did not install",
			log:  `FATAL:  could not access file "pg_stat_statements": No such file or directory`,
			want: "pg_stat_statements",
		},
		{
			name: "the other spelling, with a resolved path",
			log:  `FATAL:  could not load library "/usr/lib/postgresql/17/lib/pg_cron.so": cannot open shared object file`,
			want: "/usr/lib/postgresql/17/lib/pg_cron.so",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := missingPreloadLibrary(tc.log)
			if !ok {
				t.Fatalf("not recognised as a host-side missing library:\n%s", tc.log)
			}
			if got != tc.want {
				t.Errorf("library = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMissingPreloadLibrary_DataProblemsStayLoud is the half that
// protects the check's purpose. Each of these says something about the
// restored data; classifying any of them as "the host is missing
// something" would turn a corrupt restore into a cheerful WARN.
func TestMissingPreloadLibrary_DataProblemsStayLoud(t *testing.T) {
	for _, log := range []string{
		`FATAL:  could not locate required checkpoint record at 0/3000028`,
		`FATAL:  database files are incompatible with server`,
		`FATAL:  data directory "/var/lib/pg/data" has invalid permissions`,
		`PANIC:  could not locate a valid checkpoint record`,
		`FATAL:  the database system is starting up`,
		`LOG:  invalid primary checkpoint record`,
		`FATAL:  could not open file "pg_wal/000000010000000000000003": No such file or directory`,
		"",
	} {
		if lib, ok := missingPreloadLibrary(log); ok {
			t.Errorf("a data-level failure was softened into a host problem (%q):\n%s", lib, log)
		}
	}
}
