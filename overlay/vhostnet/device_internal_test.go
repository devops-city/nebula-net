package vhostnet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncateBuffers(t *testing.T) {
	tests := []struct {
		name     string
		buffers  [][]byte
		length   int
		expected [][]byte
	}{
		{
			name:     "no buffers",
			buffers:  nil,
			length:   0,
			expected: nil,
		},
		{
			name: "single buffer correct length",
			buffers: [][]byte{
				make([]byte, 100),
			},
			length: 100,
			expected: [][]byte{
				make([]byte, 100),
			},
		},
		{
			name: "single buffer truncated",
			buffers: [][]byte{
				make([]byte, 100),
			},
			length: 90,
			expected: [][]byte{
				make([]byte, 90),
			},
		},
		{
			name: "multiple buffers correct length",
			buffers: [][]byte{
				make([]byte, 200),
				make([]byte, 100),
			},
			length: 300,
			expected: [][]byte{
				make([]byte, 200),
				make([]byte, 100),
			},
		},
		{
			name: "multiple buffers truncated",
			buffers: [][]byte{
				make([]byte, 200),
				make([]byte, 100),
			},
			length: 250,
			expected: [][]byte{
				make([]byte, 200),
				make([]byte, 50),
			},
		},
		{
			name: "multiple buffers truncated buffer list",
			buffers: [][]byte{
				make([]byte, 200),
				make([]byte, 200),
				make([]byte, 200),
			},
			length: 350,
			expected: [][]byte{
				make([]byte, 200),
				make([]byte, 150),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := truncateBuffers(tt.buffers, tt.length)
			assert.Equal(t, tt.expected, actual)
		})
	}
}
