//go:build podman_integration

package container

import (
	"context"
	"testing"
	"time"
)

// This live check only inspects an already-installed Podman. It never pulls,
// builds, or runs an image.
func TestLivePodmanProbeNoDownloads(t *testing.T) {
	path, err := FindPodman()
	if err != nil {
		t.Skip("podman is not installed")
	}
	adapter, err := New(Config{PodmanPath: path, ProbeTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	report := adapter.Probe(context.Background())
	if !report.Podman.Available {
		t.Fatalf("podman probe failed: %#v", report)
	}
}
