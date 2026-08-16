package featureflags

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOverrideBoolFlagFromEnv(t *testing.T) {
	flag := NewBoolFlag("test-env-bool-override", false)
	t.Setenv("TEST_BOOL_OVERRIDE", " true ")

	applied, err := OverrideBoolFlagFromEnv(flag, "TEST_BOOL_OVERRIDE")
	require.NoError(t, err)
	require.True(t, applied)

	client, err := NewClient()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(t.Context())) })
	require.True(t, client.BoolFlag(t.Context(), flag))
}

func TestOverrideBoolFlagFromEnvRejectsInvalidValue(t *testing.T) {
	flag := NewBoolFlag("test-invalid-env-bool-override", false)
	t.Setenv("TEST_INVALID_BOOL_OVERRIDE", "enabled")

	applied, err := OverrideBoolFlagFromEnv(flag, "TEST_INVALID_BOOL_OVERRIDE")
	require.Error(t, err)
	require.False(t, applied)
}

func TestOverrideBoolFlagFromEnvIgnoresUnsetVariable(t *testing.T) {
	flag := NewBoolFlag("test-unset-env-bool-override", false)

	applied, err := OverrideBoolFlagFromEnv(flag, "TEST_UNSET_BOOL_OVERRIDE")
	require.NoError(t, err)
	require.False(t, applied)
}
