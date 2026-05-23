package eventbus_test

import (
	"context"
	"os"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/eventbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoop_Publish(t *testing.T) {
	n := eventbus.Noop{}
	err := n.Publish(context.Background(), "cms.articles.create", []byte(`{"id":1}`))
	assert.NoError(t, err)
}

func TestNoop_Close(t *testing.T) {
	n := eventbus.Noop{}
	assert.NoError(t, n.Close())
}

func TestNoop_ImplementsPublisher(t *testing.T) {
	var p eventbus.Publisher = eventbus.Noop{}
	assert.NotNil(t, p)
}

func TestNewFromEnv_Noop(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{"empty env var", ""},
		{"explicit noop", "noop"},
		{"noop with whitespace", "  noop  "},
		{"noop uppercase", "NOOP"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				os.Unsetenv("EVENT_BUS")
			} else {
				os.Setenv("EVENT_BUS", tt.env)
				defer os.Unsetenv("EVENT_BUS")
			}

			p, err := eventbus.NewFromEnv()
			require.NoError(t, err)
			require.NotNil(t, p)

			err = p.Publish(context.Background(), "test.topic", nil)
			assert.NoError(t, err)
		})
	}
}

// A plugin may serve a broker named by EVENT_BUS, so the engine boots on
// those values and leaves the publishing to it.
func TestNewFromEnv_BrokerBackendsAreThePluginsToServe(t *testing.T) {
	for _, b := range []string{"nats", "kafka", "rabbitmq"} {
		t.Run(b, func(t *testing.T) {
			os.Setenv("EVENT_BUS", b)
			defer os.Unsetenv("EVENT_BUS")

			p, err := eventbus.NewFromEnv()
			require.NoError(t, err, "the engine must still boot")
			require.NotNil(t, p)
			assert.NoError(t, p.Publish(context.Background(), "test.topic", nil),
				"core's bus stays a noop; the plugin does the publishing")
		})
	}
}

func TestNewFromEnv_UnknownBackend(t *testing.T) {
	os.Setenv("EVENT_BUS", "zmq")
	defer os.Unsetenv("EVENT_BUS")

	p, err := eventbus.NewFromEnv()
	assert.Error(t, err)
	assert.Nil(t, p)
	assert.Contains(t, err.Error(), "unknown")
}
