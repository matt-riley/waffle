package providerconfig

import "testing"

func TestPresetListValidatesSupportedTypesAndBaseURLRequirements(t *testing.T) {
	presets := Presets()
	if len(presets) != 4 {
		t.Fatalf("presets = %#v, want four supported choices", presets)
	}

	compatible, err := ResolvePreset("openai-compatible", "https://gateway.example/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if compatible.RuntimeType != "openai" || compatible.BaseURL != "https://gateway.example/v1" || !compatible.RequiresBaseURL {
		t.Fatalf("compatible preset = %#v", compatible)
	}
	if _, err := ResolvePreset("openai-compatible", ""); err == nil {
		t.Fatal("openai-compatible accepted an absent base URL")
	}
	if _, err := ResolvePreset("not-a-provider", ""); err == nil {
		t.Fatal("unsupported preset was accepted")
	}
}
