package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestModelAvailabilityPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "availability.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}

	offlineAt := time.Unix(1_800_000_000, 0)
	if err := db.TakeModelOffline(ctx, "provider/model", offlineAt); err != nil {
		t.Fatalf("take model offline: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	models, err := db.OfflineModels(ctx)
	if err != nil {
		t.Fatalf("list offline models: %v", err)
	}
	if len(models) != 1 || models[0].Model != "provider/model" || !models[0].OfflineAt.Equal(offlineAt) {
		t.Fatalf("offline models after reopen = %+v", models)
	}
	if models[0].ProbeFailures != 0 || !models[0].NextProbeAt.Equal(offlineAt) {
		t.Fatalf("offline model schedule = %+v", models[0])
	}

	nextProbe := offlineAt.Add(5 * time.Minute)
	if err := db.RecordModelProbeFailure(ctx, "provider/model", nextProbe); err != nil {
		t.Fatalf("record failed model probe: %v", err)
	}
	due, err := db.DueModelProbes(ctx, offlineAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("models due before next probe = %+v, want none", due)
	}
	due, err = db.DueModelProbes(ctx, nextProbe)
	if err != nil || len(due) != 1 || due[0].ProbeFailures != 1 {
		t.Fatalf("models due at next probe = %+v (%v), want one failed probe", due, err)
	}

	if err := db.RestoreModel(ctx, "provider/model"); err != nil {
		t.Fatalf("restore model: %v", err)
	}
	models, err = db.OfflineModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("offline models after restore = %+v, want none", models)
	}
}
