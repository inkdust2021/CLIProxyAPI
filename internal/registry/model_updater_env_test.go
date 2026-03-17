package registry

import "testing"

func TestModelsRefreshEnabledDefaultsToFalse(t *testing.T) {
	t.Setenv(modelsRefreshEnabledEnv, "")
	if modelsRefreshEnabled() {
		t.Fatal("expected remote model refresh to be disabled by default")
	}
}

func TestModelsRefreshEnabledAcceptsTruthyValues(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(modelsRefreshEnabledEnv, value)
		if !modelsRefreshEnabled() {
			t.Fatalf("expected %q to enable remote model refresh", value)
		}
	}
}

func TestModelsRefreshEnabledRejectsFalseyValues(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off", "random"} {
		t.Setenv(modelsRefreshEnabledEnv, value)
		if modelsRefreshEnabled() {
			t.Fatalf("expected %q to keep remote model refresh disabled", value)
		}
	}
}
