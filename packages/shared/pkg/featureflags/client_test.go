package featureflags

import (
	"context"
	"testing"

	"github.com/launchdarkly/go-sdk-common/v3/ldcontext"
	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
	"github.com/launchdarkly/go-server-sdk/v7/testhelpers/ldtestdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	flagName = "demo-feature-flag"
)

func TestOfflineDatastore(t *testing.T) {
	t.Parallel()
	clientCtx := ldcontext.NewBuilder(flagName).Build()
	client, err := NewClient()
	require.NoError(t, err)

	t.Cleanup(func() {
		err = client.Close(context.WithoutCancel(t.Context()))
		assert.NoError(t, err)
	})

	// value is not set so it should be default (false)
	flagValue, _ := client.ld.BoolVariation(flagName, clientCtx, false)
	assert.False(t, flagValue)

	launchDarklyOfflineStore.Update(
		launchDarklyOfflineStore.Flag(flagName).VariationForAll(true),
	)
	// Restore so the package survives -count=N reruns in one process.
	t.Cleanup(func() {
		launchDarklyOfflineStore.Update(
			launchDarklyOfflineStore.Flag(flagName).VariationForAll(false),
		)
	})

	// value is set manually in datastore and should be taken from there
	flagValue, _ = client.ld.BoolVariation(flagName, clientCtx, false)
	assert.True(t, flagValue)
}

func TestAllContextsIncludesServiceAndDeployment(t *testing.T) {
	t.Parallel()

	client := &Client{}
	client.SetDeploymentName("dev")
	client.SetServiceName("orchestration-api")

	merged := mergeContexts(t.Context(), client.allContexts(t.Context(), nil))
	contexts := merged.GetAllIndividualContexts(nil)

	seen := map[ldcontext.Kind]string{}
	for _, item := range contexts {
		seen[item.Kind()] = item.Key()
	}

	require.Equal(t, "dev", seen[deploymentKind])
	require.Equal(t, "orchestration-api", seen[ServiceKind])
}

func TestAllContextsIncludesRegisteredProviders(t *testing.T) {
	t.Parallel()

	client := &Client{}
	client.RegisterContextProvider(func(context.Context) ldcontext.Context {
		return ldcontext.NewWithKind("node", "node-1")
	})

	merged := mergeContexts(t.Context(), client.allContexts(t.Context(), nil))
	contexts := merged.GetAllIndividualContexts(nil)

	seen := map[ldcontext.Kind]string{}
	for _, item := range contexts {
		seen[item.Kind()] = item.Key()
	}

	require.Equal(t, "node-1", seen["node"])
}

func TestClickhouseAsyncOfflineFallback(t *testing.T) {
	t.Parallel()
	client, err := NewClientWithDatasource(launchDarklyOfflineStore)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.WithoutCancel(t.Context()))) })
	_, ok := client.BoolFlagOverride(t.Context(), ClickhouseAsyncInsertFlag, BatcherContext("sandbox-events"))
	require.False(t, ok, "offline defaults must not override writer-specific async behavior")
	require.True(t, client.BoolFlag(t.Context(), ClickhouseAsyncInsertFlag))
	require.True(t, client.BoolFlag(t.Context(), ClickhouseWaitForAsyncInsertFlag))
}

func TestIntFlagOverride(t *testing.T) {
	t.Parallel()

	const fallback = -1

	tests := []struct {
		name       string
		serve      *ldvalue.Value
		wantValue  int
		wantServed bool
	}{
		{name: "served value", serve: new(ldvalue.Int(25)), wantValue: 25, wantServed: true},
		{name: "served value equal to the fallback", serve: new(ldvalue.Int(fallback)), wantValue: fallback, wantServed: true},
		{name: "key the environment does not define", serve: nil, wantValue: fallback, wantServed: true},
		{name: "wrong type", serve: new(ldvalue.String("25")), wantValue: fallback, wantServed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			source := ldtestdata.DataSource()
			flag := IntFlag{name: "int-flag-override-test", fallback: fallback}
			if tt.serve != nil {
				source.Update(source.Flag(flag.Key()).ValueForAll(*tt.serve))
			}

			client, err := NewClientWithDatasource(source)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close(context.WithoutCancel(t.Context()))) })

			value, served := client.IntFlagOverride(t.Context(), flag)
			assert.Equal(t, tt.wantValue, value)
			assert.Equal(t, tt.wantServed, served)
		})
	}
}

func TestIntFlagOverrideNilClient(t *testing.T) {
	t.Parallel()

	value, served := (&Client{}).IntFlagOverride(t.Context(), IntFlag{name: "int-flag-override-test", fallback: 7})
	assert.Equal(t, 7, value)
	assert.False(t, served)
}
