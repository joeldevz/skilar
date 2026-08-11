package reporter

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/runner"
)

func TestLegacyResultJSONRoundTripUsesPrivateAtomicStorage(t *testing.T) {
	result := &runner.SuiteResult{Timestamp: time.Unix(1, 0).UTC(), SuiteName: "suite", TotalCases: 1, PassCount: 1, PassRate: 1}
	path := filepath.Join(t.TempDir(), "result.json")
	if err := SaveResult(result, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := LoadResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SuiteName != result.SuiteName || loaded.PassRate != 1 || !loaded.Timestamp.Equal(result.Timestamp) {
		t.Fatalf("round trip = %+v", loaded)
	}
}
