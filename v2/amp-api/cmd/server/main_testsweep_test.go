package main

// The suite sweeps its own Typesense namespace when it ends.
//
// Every KB collection this suite creates lives under one prefix for the run
// (kb.SetCollectionPrefix, wired in testKB), so it can run against the same
// Typesense a live instance uses without creating, emptying or dropping that
// instance's collections. Sweeping that prefix at the end is the other half:
// a run leaves the instance as it found it instead of accumulating a
// collection per project, team and org it touched (#3689).
//
// Only the run's own prefix is swept, and kb.DropTestCollections refuses
// anything outside the test namespace outright, so a wiring mistake can never
// turn this into a delete of live knowledge. A test that fails with its own
// collections still on the server is fine: the sweep is best effort, and a
// later run never sees them (its prefix is different).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/simstech/amp-api/internal/kb"
)

func TestMain(m *testing.M) {
	code := m.Run()

	if url := os.Getenv("TEST_TYPESENSE_URL"); url != "" && kb.IsTestCollectionName(testKBNamespace()) {
		key := os.Getenv("TEST_TYPESENSE_KEY")
		if key == "" {
			key = "test"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := kb.New(url, key, "", "").DropTestCollections(ctx, testKBNamespace()); err != nil {
			// A test that failed this way is worth seeing; this is cleanup.
			println("sweep the test KB namespace:", err.Error())
		}
		cancel()
	}
	os.Exit(code)
}
