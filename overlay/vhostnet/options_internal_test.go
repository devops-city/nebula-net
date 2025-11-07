package vhostnet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOptionValues_Apply(t *testing.T) {
	opts := optionDefaults
	opts.apply([]Option{
		WithQueueSize(256),
		WithBackendFD(99),
	})

	assert.Equal(t, optionValues{
		queueSize: 256,
		backendFD: 99,
	}, opts)
}

func TestOptionValues_Validate(t *testing.T) {
	tests := []struct {
		name      string
		values    optionValues
		assertErr assert.ErrorAssertionFunc
	}{
		{
			name: "queue size missing",
			values: optionValues{
				queueSize: -1,
				backendFD: 99,
			},
			assertErr: assert.Error,
		},
		{
			name: "invalid queue size",
			values: optionValues{
				queueSize: 24,
				backendFD: 99,
			},
			assertErr: assert.Error,
		},
		{
			name: "backend fd missing",
			values: optionValues{
				queueSize: 256,
				backendFD: -1,
			},
			assertErr: assert.Error,
		},
		{
			name: "valid",
			values: optionValues{
				queueSize: 256,
				backendFD: 99,
			},
			assertErr: assert.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.assertErr(t, tt.values.validate())
		})
	}
}
