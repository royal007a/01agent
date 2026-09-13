package main

import "testing"

func TestProviderFromEnvDoesNotReturnTypedNil(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			t.Setenv("AGENT_PROVIDER", protocol)
			t.Setenv("AGENT_API_KEY", "")
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("ANTHROPIC_API_KEY", "")

			model, err := providerFromEnv()
			if err == nil {
				t.Fatal("providerFromEnv() error = nil, want missing-key error")
			}
			if model != nil {
				t.Fatalf("providerFromEnv() model = %#v, want nil", model)
			}
		})
	}
}
