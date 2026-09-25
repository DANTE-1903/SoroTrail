package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSamplerFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		samplerName string
		argument    string
		wantParts   []string
	}{
		{name: "always on", samplerName: "always_on", wantParts: []string{"AlwaysOnSampler"}},
		{name: "always off", samplerName: "always_off", wantParts: []string{"AlwaysOffSampler"}},
		{name: "parent based always on", samplerName: "parentbased_always_on", wantParts: []string{"ParentBased", "AlwaysOnSampler"}},
		{name: "parent based always off", samplerName: "parentbased_always_off", wantParts: []string{"ParentBased", "AlwaysOffSampler"}},
		{name: "ratio sampler", samplerName: "traceidratio", argument: "0.25", wantParts: []string{"ParentBased", "0.25"}},
		{name: "parent based ratio sampler", samplerName: "parentbased_traceidratio", argument: "0.25", wantParts: []string{"ParentBased", "0.25"}},
		{name: "unset sampler uses baseline", wantParts: []string{"ParentBased", "{1}"}},
		{name: "unknown sampler uses baseline", samplerName: "not-a-sampler", wantParts: []string{"ParentBased", "{1}"}},
		{name: "out of range ratio is clamped", samplerName: "traceidratio", argument: "2.5", wantParts: []string{"ParentBased", "{1}"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", tt.argument)
			sampler := samplerFromEnv(tt.samplerName)
			require.NotNil(t, sampler)
			description := sampler.Description()
			for _, part := range tt.wantParts {
				assert.Contains(t, description, part)
			}
		})
	}
}
