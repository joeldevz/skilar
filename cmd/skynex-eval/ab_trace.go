package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

type abStepStatus string

const (
	abStepStatusSuccess abStepStatus = "success"
	abStepStatusError   abStepStatus = "error"
	abStepStatusSkipped abStepStatus = "skipped"
)

type abStepReport struct {
	Name   string       `json:"name"`
	Status abStepStatus `json:"status"`
	Detail string       `json:"detail,omitempty"`
}

type abExecutionTrace struct {
	RunID         string
	CorrelationID string
	steps         []abStepReport
	index         map[string]int
}

var abStepOrder = []string{"setup", "sampling", "finalization", "cleanup"}

func newABExecutionTrace() abExecutionTrace {
	trace := abExecutionTrace{
		RunID:         newABTraceID("run"),
		CorrelationID: newABTraceID("corr"),
		steps:         make([]abStepReport, len(abStepOrder)),
		index:         make(map[string]int, len(abStepOrder)),
	}
	for i, name := range abStepOrder {
		trace.steps[i] = abStepReport{Name: name, Status: abStepStatusSkipped, Detail: "not reached"}
		trace.index[name] = i
	}
	return trace
}

func newABTraceID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("generate %s trace id: %v", prefix, err))
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func (t *abExecutionTrace) markSuccess(step string) {
	if t == nil {
		return
	}
	if index, ok := t.index[step]; ok {
		t.steps[index].Status = abStepStatusSuccess
		t.steps[index].Detail = ""
	}
}

func (t *abExecutionTrace) markError(step string, err error) {
	if t == nil {
		return
	}
	if index, ok := t.index[step]; ok {
		t.steps[index].Status = abStepStatusError
		t.steps[index].Detail = safeDiagnostic(err)
		for i := index + 1; i < len(t.steps); i++ {
			t.steps[i].Status = abStepStatusSkipped
			t.steps[i].Detail = "not reached after " + step
		}
	}
}

func (t *abExecutionTrace) markSkipped(step, reason string) {
	if t == nil {
		return
	}
	if index, ok := t.index[step]; ok {
		t.steps[index].Status = abStepStatusSkipped
		if reason == "" {
			reason = "not reached"
		}
		t.steps[index].Detail = reason
	}
}

func (t *abExecutionTrace) snapshot() []abStepReport {
	if t == nil {
		return nil
	}
	return append([]abStepReport(nil), t.steps...)
}
