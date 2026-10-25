package faultinject_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
)

type bareBackend struct{ storage.StoragePlugin }

// The middleware must forward the optional capabilities, or a gameday
// wrapping the fs plugin silently skips the capacity gate and the reaper.
func TestFaultInject_FreeSpaceAndReapForwardToFS(t *testing.T) {
	mw := faultinject.New(fsBackend(t))
	bare, err := storage.FreeSpaceOf(context.Background(), fsBackend(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bare.Unsupported {
		got, err := mw.FreeSpace(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.Unsupported || got.TotalBytes == 0 {
			t.Fatalf("free space hidden by the middleware: %+v", got)
		}
	}
	rs, err := mw.ReapStaging(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Unsupported {
		t.Fatal("middleware hid the fs staging reaper")
	}
}

func TestFaultInject_CapabilitiesUnsupportedOnBareBackend(t *testing.T) {
	mw := faultinject.New(bareBackend{})
	fsInfo, err := mw.FreeSpace(context.Background())
	if err != nil || !fsInfo.Unsupported {
		t.Fatalf("FreeSpace on a bare backend: %+v, %v", fsInfo, err)
	}
	rs, err := mw.ReapStaging(context.Background(), time.Hour)
	if err != nil || !rs.Unsupported {
		t.Fatalf("ReapStaging on a bare backend: %+v, %v", rs, err)
	}
}
