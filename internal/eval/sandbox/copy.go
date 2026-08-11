package sandbox

import (
	"errors"
	"fmt"
	"io"

	"github.com/joeldevz/skynex/internal/safefs"
)

// CopyVerifiedTree securely copies a trusted tree into an existing empty
// destination and proves source and destination digests match. It is suitable
// for freezing harness/control/candidate bundles without materializing a run.
func CopyVerifiedTree(sourcePath, destinationPath string, limits SnapshotLimits) (Snapshot, error) {
	if err := validateAbsoluteDir(sourcePath); err != nil {
		return Snapshot{}, fmt.Errorf("invalid source: %w", err)
	}
	if err := validateAbsoluteDir(destinationPath); err != nil {
		return Snapshot{}, fmt.Errorf("invalid destination: %w", err)
	}
	limits, err := normalizeSnapshotLimits(limits)
	if err != nil {
		return Snapshot{}, err
	}
	source, err := DigestTree(sourcePath, limits)
	if err != nil {
		return Snapshot{}, err
	}
	destinationRoot, err := safefs.Open(destinationPath)
	if err != nil {
		return Snapshot{}, err
	}
	defer destinationRoot.Close()
	dir, err := destinationRoot.Open(".")
	if err != nil {
		return Snapshot{}, err
	}
	entries, readErr := dir.ReadDir(1)
	closeErr := dir.Close()
	if readErr == nil || len(entries) != 0 {
		return Snapshot{}, fmt.Errorf("destination must be empty")
	}
	if !errors.Is(readErr, io.EOF) {
		return Snapshot{}, fmt.Errorf("inspect destination: %w", readErr)
	}
	if closeErr != nil {
		return Snapshot{}, closeErr
	}
	if err := copyVerifiedTree(sourcePath, destinationRoot, source, limits); err != nil {
		return Snapshot{}, err
	}
	destination, err := takeSnapshot(destinationRoot, limits)
	if err != nil {
		return Snapshot{}, err
	}
	if destination.Digest != source.Digest {
		return Snapshot{}, fmt.Errorf("destination digest mismatch: got %s, expected %s", destination.Digest, source.Digest)
	}
	sourceAfter, err := DigestTree(sourcePath, limits)
	if err != nil {
		return Snapshot{}, err
	}
	if sourceAfter.Digest != source.Digest {
		return Snapshot{}, fmt.Errorf("source changed while copying: got %s, expected %s", sourceAfter.Digest, source.Digest)
	}
	return destination, nil
}
