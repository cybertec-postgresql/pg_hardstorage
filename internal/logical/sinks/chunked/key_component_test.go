package chunked_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// Deployment and stream name become components of every key the sink
// writes (logical/<deployment>/<stream>/...). A name that is not a single
// component must be refused before anything is written.
func TestNew_RejectsNamesThatEscapeTheLogicalPrefix(t *testing.T) {
	sp := openFS(t, "file://"+t.TempDir())
	defer sp.Close()
	for _, c := range []struct{ dep, stream string }{
		{"../audit", "events"},
		{"db1", "../../chunks"},
		{"..", "events"},
		{"db1", "."},
		{"a/b", "events"},
		{"db1", `a\b`},
		{"db1", "ev\nents"},
	} {
		if _, err := chunked.New(casdefault.New(sp), sp, chunked.Options{
			Deployment: c.dep, StreamName: c.stream, Slot: "slot1",
		}); err == nil {
			t.Errorf("New(deployment=%q, stream=%q) accepted a name that escapes logical/", c.dep, c.stream)
		}
	}
	// Names that are one component stay legal, dots and spaces included.
	for _, c := range []struct{ dep, stream string }{{"db1", "events"}, {"db.prod", "order events"}} {
		if _, err := chunked.New(casdefault.New(sp), sp, chunked.Options{
			Deployment: c.dep, StreamName: c.stream, Slot: "slot1",
		}); err != nil {
			t.Errorf("New(deployment=%q, stream=%q): %v", c.dep, c.stream, err)
		}
	}
}
