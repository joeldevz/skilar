package main

import (
	"strings"
	"testing"
)

func TestEvaluatorManagedDetachEnvironmentIsExact(t *testing.T) {
	t.Setenv(evaluatorManagedDetachEnvironment, "1")
	managed, err := evaluatorManagedDetachMode()
	if err != nil || !managed {
		t.Fatalf("managed detach = %t, err = %v", managed, err)
	}

	for _, value := range []string{"", "0", "true", " 1", "1 "} {
		t.Run("reject_"+strings.ReplaceAll(value, " ", "space"), func(t *testing.T) {
			t.Setenv(evaluatorManagedDetachEnvironment, value)
			if managed, err := evaluatorManagedDetachMode(); err == nil || managed || !strings.Contains(err.Error(), evaluatorManagedDetachEnvironment) {
				t.Fatalf("value %q = %t, %v", value, managed, err)
			}
		})
	}
}
