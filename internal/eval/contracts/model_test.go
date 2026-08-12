package contracts

import (
	"strings"
	"testing"
)

func TestParseModelSelectionUsesOneContractAcrossLayers(t *testing.T) {
	provider, model, err := ParseModelSelection("vertex/models/gemini/2.5")
	if err != nil || provider != "vertex" || model != "models/gemini/2.5" {
		t.Fatalf("nested model selection = %q/%q, err=%v", provider, model, err)
	}
	for _, invalid := range []string{
		"", "provider", "/model", "provider/", "provider/model with-space",
		"provider\t/model", "provider/model\n", "provider/modèle", strings.Repeat("x", MaxModelSelectionBytes+1),
	} {
		if _, _, err := ParseModelSelection(invalid); err == nil {
			t.Fatalf("invalid model selection was accepted: %q", invalid)
		}
	}
}
