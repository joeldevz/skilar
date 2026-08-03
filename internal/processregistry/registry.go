package processregistry

import (
	"context"
	"sync"
)

var registry = struct {
	sync.Mutex
	items map[string]map[string]context.CancelFunc
}{items: map[string]map[string]context.CancelFunc{}}

func Register(workflowID, invocationID string, cancel context.CancelFunc) func() {
	registry.Lock()
	defer registry.Unlock()
	if registry.items[workflowID] == nil {
		registry.items[workflowID] = map[string]context.CancelFunc{}
	}
	registry.items[workflowID][invocationID] = cancel
	return func() {
		registry.Lock()
		defer registry.Unlock()
		delete(registry.items[workflowID], invocationID)
		if len(registry.items[workflowID]) == 0 {
			delete(registry.items, workflowID)
		}
	}
}
func CancelWorkflow(workflowID string) int {
	registry.Lock()
	items := registry.items[workflowID]
	delete(registry.items, workflowID)
	registry.Unlock()
	for _, cancel := range items {
		cancel()
	}
	return len(items)
}
