//go:build !linux

package workflow

// Some supported platforms lack a portable statfs API. The write probe remains
// authoritative there; callers can inject AvailableBytes when they need a
// stricter policy.
func runtimeAvailableBytes(string) (uint64, error) { return ^uint64(0), nil }
