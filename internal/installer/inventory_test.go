package installer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListSnapshotsSortsTrustedCreatedAtAndReportsSafety(t *testing.T) {
	state := t.TempDir()
	base := filepath.Join(state, "snapshots")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSnapshotFixture(t, base, "00000000000000000000000000000002", `{"version":1,"createdAt":"2026-01-02T00:00:00Z","status":"committed","entries":[{"path":"x","kind":"file"}]}`, "payload")
	writeSnapshotFixture(t, base, "00000000000000000000000000000001", `{"version":1,"createdAt":"2026-01-01T00:00:00Z","status":"recovery-needed","entries":[]}`, "")
	items, err := ListSnapshots(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "00000000000000000000000000000001" {
		t.Fatalf("inventory order = %#v", items)
	}
	if items[1].Size == 0 || items[1].FileCount == 0 || !items[1].EligibleToPrune || !items[1].Restorable {
		t.Fatalf("committed inventory = %#v", items[1])
	}
	if items[0].EligibleToPrune {
		t.Fatal("recovery snapshot was eligible to prune")
	}
}

func TestListSnapshotsTreatsRestorableV1WithoutStatusAsPrunableLegacy(t *testing.T) {
	state := t.TempDir()
	base := filepath.Join(state, "snapshots")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSnapshotFixture(t, base, "00000000000000000000000000000001", `{"version":1,"entries":[{"path":"x","kind":"file"}]}`, "payload")

	items, err := ListSnapshots(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "legacy" || !items[0].Restorable || !items[0].EligibleToPrune {
		t.Fatalf("legacy inventory = %#v", items)
	}
}

func writeSnapshotFixture(t *testing.T, base, id, manifest, payload string) {
	t.Helper()
	dir := filepath.Join(base, id)
	if err := os.MkdirAll(filepath.Join(dir, "payload"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if payload != "" {
		if err := os.WriteFile(filepath.Join(dir, "payload", "data"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
