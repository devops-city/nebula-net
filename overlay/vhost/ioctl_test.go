package vhost_test

import (
	"testing"
	"unsafe"

	"github.com/hetznercloud/virtio-go/vhost"
	"github.com/stretchr/testify/assert"
)

func TestQueueState_Size(t *testing.T) {
	assert.EqualValues(t, 8, unsafe.Sizeof(vhost.QueueState{}))
}

func TestQueueAddresses_Size(t *testing.T) {
	assert.EqualValues(t, 40, unsafe.Sizeof(vhost.QueueAddresses{}))
}

func TestQueueFile_Size(t *testing.T) {
	assert.EqualValues(t, 8, unsafe.Sizeof(vhost.QueueFile{}))
}
