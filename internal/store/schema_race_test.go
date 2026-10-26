package store

import (
	"path/filepath"
	"sync"
	"testing"
)

// Two processes starting together on a stale database must not both apply the
// same migration step. The loser used to read user_version before taking the
// write lock, replay the ALTER the winner had already committed, and fail to
// start at all with "duplicate column name".
func TestConcurrentOpenDBMigratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	writeV1Fixture(t, path)

	const openers = 6
	var wg sync.WaitGroup
	errs := make([]error, openers)
	start := make(chan struct{})

	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			d, err := OpenDB(path)
			if err == nil {
				_ = d.Close()
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent OpenDB #%d failed: %v", i, err)
		}
	}

	d, err := OpenDB(path)
	if err != nil {
		t.Fatalf("reopen after the race: %v", err)
	}
	defer func() { _ = d.Close() }()
	var version int
	if err := d.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}
}
