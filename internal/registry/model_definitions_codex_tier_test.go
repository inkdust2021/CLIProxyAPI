package registry

import "testing"

func TestCodexTierModelsIncludeRequiredGPT5Models(t *testing.T) {
	t.Parallel()

	tierModels := map[string][]*ModelInfo{
		"free": GetCodexFreeModels(),
		"team": GetCodexTeamModels(),
		"plus": GetCodexPlusModels(),
		"pro":  GetCodexProModels(),
	}

	required := []string{"gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini"}
	for tier, models := range tierModels {
		for _, modelID := range required {
			if !hasModelID(models, modelID) {
				t.Fatalf("expected codex-%s to include %q", tier, modelID)
			}
		}
	}
}

func hasModelID(models []*ModelInfo, modelID string) bool {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return true
		}
	}
	return false
}
