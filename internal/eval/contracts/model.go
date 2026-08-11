package contracts

import (
	"fmt"
	"strings"
)

// MaxModelSelectionBytes is shared by case, manifest, doctor, and runtime
// validation so a provider/model value cannot be accepted by one layer and
// rejected later by another.
const MaxModelSelectionBytes = 256

// ParseModelSelection validates provider/model while allowing additional '/'
// characters inside the model ID (for example vertex/models/gemini/2.5).
func ParseModelSelection(selection string) (providerID, modelID string, err error) {
	if len(selection) == 0 || len(selection) > MaxModelSelectionBytes {
		return "", "", fmt.Errorf("model selection must contain between 1 and %d bytes", MaxModelSelectionBytes)
	}
	for index := 0; index < len(selection); index++ {
		if selection[index] < 0x21 || selection[index] > 0x7e {
			return "", "", fmt.Errorf("model selection must contain printable ASCII without whitespace")
		}
	}
	providerID, modelID, ok := strings.Cut(selection, "/")
	if !ok || providerID == "" || modelID == "" {
		return "", "", fmt.Errorf("model selection must use provider/model form")
	}
	return providerID, modelID, nil
}
