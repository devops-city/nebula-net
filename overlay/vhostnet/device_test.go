package vhostnet_test

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/gopacket/gopacket/afpacket"
	"github.com/hetznercloud/virtio-go/internal/testsupport"
	"github.com/hetznercloud/virtio-go/tuntap"
	"github.com/hetznercloud/virtio-go/vhostnet"
	"github.com/hetznercloud/virtio-go/virtio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Here is the general idea of how the following tests work to verify the
// correct communication with the vhost-net device within the kernel:
//
//   +-----------------------------------+
//   |   go test running in user space   |
//   +-----------------------------------+
//            ^                    ^
//            |                    |
//    capture / write      transmit / receive
//    using AF_PACKET      using this package
//            |                    |
//            v                    v
//   +----------------+      +-----------+
//   | tun (TAP mode) |<---->| vhost-net |
//   +----------------+      +-----------+
//

func TestDevice_TransmitPacket(t *testing.T) {
	testsupport.VirtrunOnly(t)

	fx := NewTestFixture(t)

	for _, length := range []int{64, 1514, 9014, 64100} {
		t.Run(fmt.Sprintf("%d byte packet", length), func(t *testing.T) {
			vnethdr, pkt := testsupport.TestPacket(t, fx.TAPDevice.MAC(), length)

			// Transmit the packet over the vhost-net device.
			require.NoError(t, fx.NetDevice.TransmitPacket(vnethdr, pkt))

			// Check if the packet arrived at the TAP device. The virtio-net
			// header should have been stripped by the TAP device.
			data, _, err := fx.TPacket.ReadPacketData()
			assert.NoError(t, err)
			assert.Equal(t, pkt, data)
		})
	}
}

func TestDevice_ReceivePacket(t *testing.T) {
	testsupport.VirtrunOnly(t)

	fx := NewTestFixture(t)

	for _, length := range []int{64, 1514, 9014, 64100} {
		t.Run(fmt.Sprintf("%d byte packet", length), func(t *testing.T) {
			vnethdr, pkt := testsupport.TestPacket(t, fx.TAPDevice.MAC(), length)
			prependedPkt := testsupport.PrependPacket(t, vnethdr, pkt)

			// Write the prepended packet to the TAP device.
			require.NoError(t, fx.TPacket.WritePacketData(prependedPkt))

			// Try to receive the packet on the vhost-net device.
			vnethdr, data, err := fx.NetDevice.ReceivePacket()
			assert.NoError(t, err)
			assert.Equal(t, pkt, data)

			// Large packets should have been received as multiple buffers.
			assert.Equal(t, (len(prependedPkt)/os.Getpagesize())+1, int(vnethdr.NumBuffers))
		})
	}
}

func TestDevice_TransmitManyPackets(t *testing.T) {
	testsupport.VirtrunOnly(t)

	fx := NewTestFixture(t)

	// Test with a packet which does not fit into a single memory page.
	vnethdr, pkt := testsupport.TestPacket(t, fx.TAPDevice.MAC(), 9014)

	const count = 1024
	var received int

	var wg sync.WaitGroup
	wg.Go(func() {
		for range count {
			err := fx.NetDevice.TransmitPacket(vnethdr, pkt)
			if !assert.NoError(t, err) {
				return
			}
		}
	})
	wg.Go(func() {
		for range count {
			data, _, err := fx.TPacket.ReadPacketData()
			if !assert.NoError(t, err) {
				return
			}
			assert.Equal(t, pkt, data)
			received++
		}
	})
	wg.Wait()

	assert.Equal(t, count, received)
}

func TestDevice_ReceiveManyPackets(t *testing.T) {
	testsupport.VirtrunOnly(t)

	fx := NewTestFixture(t)

	// Test with a packet which does not fit into a single memory page.
	vnethdr, pkt := testsupport.TestPacket(t, fx.TAPDevice.MAC(), 9014)
	prependedPkt := testsupport.PrependPacket(t, vnethdr, pkt)

	const count = 1024
	var received int

	var wg sync.WaitGroup
	wg.Go(func() {
		for range count {
			err := fx.TPacket.WritePacketData(prependedPkt)
			if !assert.NoError(t, err) {
				return
			}
		}
	})
	wg.Go(func() {
		for range count {
			_, data, err := fx.NetDevice.ReceivePacket()
			if !assert.NoError(t, err) {
				return
			}
			assert.Equal(t, pkt, data)
			received++
		}
	})
	wg.Wait()

	assert.Equal(t, count, received)
}

type TestFixture struct {
	TAPDevice *tuntap.Device
	NetDevice *vhostnet.Device
	TPacket   *afpacket.TPacket
}

func NewTestFixture(t *testing.T) *TestFixture {
	testsupport.VirtrunOnly(t)

	// In case something doesn't work, some more debug logging from the kernel
	// modules may be very helpful.
	testsupport.EnableDynamicDebug(t, "module tun")
	testsupport.EnableDynamicDebug(t, "module vhost")
	testsupport.EnableDynamicDebug(t, "module vhost_net")

	// Make sure the Linux kernel does not send router solicitations that may
	// interfere with these tests.
	testsupport.SetSysctl(t, "net.ipv6.conf.all.disable_ipv6", "1")

	var (
		fx  TestFixture
		err error
	)

	// Create a TAP device.
	fx.TAPDevice, err = tuntap.NewDevice(
		tuntap.WithDeviceType(tuntap.DeviceTypeTAP),
		// Helps to stop the Linux kernel from sending packets on this
		// interface.
		tuntap.WithInterfaceFlags(unix.IFF_NOARP),
		// Packets going over this device are prepended with a virtio-net
		// header. When this is not set, then packets written to the TAP device
		// will be passed to the Linux network stack without their virtio-net
		// header stripped.
		tuntap.WithVirtioNetHdr(true),
		// When writing packets into the TAP device using the RAW socket, we
		// don't want the offloads to be applied by the kernel. Advertising
		// offload support makes the kernel pass the offload request along to
		// our vhost-net device.
		tuntap.WithOffloads(unix.TUN_F_CSUM|unix.TUN_F_USO4|unix.TUN_F_USO6),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, fx.TAPDevice.Close())
	})

	// Create a vhost-net device that uses the TAP device as the backend.
	fx.NetDevice, err = vhostnet.NewDevice(
		vhostnet.WithQueueSize(32),
		vhostnet.WithBackendDevice(fx.TAPDevice),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, fx.NetDevice.Close())
	})

	// Open a RAW socket to capture packets arriving at the TAP device or
	// write packets into it.
	fx.TPacket, err = afpacket.NewTPacket(
		afpacket.SocketRaw,
		afpacket.TPacketVersion3,
		afpacket.OptInterface(fx.TAPDevice.Name()),

		// Tell the kernel that packets written to this socket are prepended
		// with a virto-net header. This is used to communicate the use of GSO
		// for large packets.
		afpacket.OptVNetHdrSize(virtio.NetHdrSize),
	)
	require.NoError(t, err)
	t.Cleanup(fx.TPacket.Close)

	return &fx
}
