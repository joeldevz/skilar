package container

import (
	"bytes"
	"sync"
)

type boundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int64
	truncated bool
	notify    chan<- struct{}
}

func newBoundedBuffer(limit int64, notify chan<- struct{}) *boundedBuffer {
	return &boundedBuffer{limit: limit, notify: notify}
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(value)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining < int64(len(value)) {
		b.truncated = true
		if remaining > 0 {
			_, _ = b.buffer.Write(value[:remaining])
		}
		select {
		case b.notify <- struct{}{}:
		default:
		}
		return original, nil
	}
	_, _ = b.buffer.Write(value)
	return original, nil
}

func (b *boundedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String(), b.truncated
}
