package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFullSyncRejectsLinkedDestinationAncestors(t *testing.T) {
	for _, component := range []string{"internal", "internal/assets", "internal/assets/data"} {
		t.Run(component, func(t *testing.T) {
			repo, outside := t.TempDir(), t.TempDir()
			link := filepath.Join(repo, component)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(repo, "internal/assets/data")
			if err := os.MkdirAll(data, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"README.md", "sentinel"} {
				if err := os.WriteFile(filepath.Join(data, name), []byte("unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := syncAllAssets(repo); err == nil {
				t.Fatal("linked destination ancestor accepted")
			}
			for _, name := range []string{"README.md", "sentinel"} {
				got, err := os.ReadFile(filepath.Join(data, name))
				if err != nil || string(got) != "unchanged" {
					t.Fatalf("outside %s changed: %q, %v", name, got, err)
				}
			}
		})
	}
}

func TestFullSyncPreservesBootstrapAndConfinesCleanup(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	data := filepath.Join(repo, "internal/assets/data")
	for _, dir := range []string{data, filepath.Join(repo, "opencode")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{
		filepath.Join(data, "README.md"):            "bootstrap",
		filepath.Join(outside, "sentinel"):          "outside",
		filepath.Join(repo, "opencode/payload.txt"): "payload",
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(data, "stale")); err != nil {
		t.Fatal(err)
	}
	if err := syncAllAssets(repo); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(data, "README.md"):            "bootstrap",
		filepath.Join(outside, "sentinel"):          "outside",
		filepath.Join(data, "opencode/payload.txt"): "payload",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(data, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale link not removed: %v", err)
	}
}

func TestFullSyncCopyUsesRetainedDestination(t *testing.T) {
	base, source, outside := t.TempDir(), t.TempDir(), t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	dest, err := openSyncTree(data)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.close()
	if err := os.Rename(data, filepath.Join(base, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyDir(source, dest, "opencode", nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(base, "retained/opencode/payload"))
	if err != nil || string(got) != "inside" {
		t.Fatalf("retained output = %q, %v", got, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("copy touched outside directory: %v, %v", entries, err)
	}
}
