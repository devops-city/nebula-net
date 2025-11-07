package tuntap_test

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/gopacket/gopacket/afpacket"
	"github.com/hetznercloud/virtio-go/internal/testsupport"
	"github.com/hetznercloud/virtio-go/tuntap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestNewDevice(t *testing.T) {
	testsupport.VirtrunOnly(t)

	t.Run("with static name", func(t *testing.T) {
		const name = "test42"
		dev, err := tuntap.NewDevice(
			tuntap.WithDeviceType(tuntap.DeviceTypeTAP),
			tuntap.WithName(name),
		)
		require.NoError(t, err)
		t.Cleanup(func() {
			assert.NoError(t, dev.Close())
		})

		assert.Equal(t, name, dev.Name())

		iface, err := net.InterfaceByIndex(int(dev.Ifindex()))
		assert.NoError(t, err)
		assert.Equal(t, name, iface.Name)
		assert.Equal(t, dev.MAC(), iface.HardwareAddr)
	})

	t.Run("with auto selected name", func(t *testing.T) {
		dev, err := tuntap.NewDevice(
			tuntap.WithDeviceType(tuntap.DeviceTypeTAP),
		)
		require.NoError(t, err)
		t.Cleanup(func() {
			assert.NoError(t, dev.Close())
		})

		assert.Contains(t, dev.Name(), "tap")

		iface, err := net.InterfaceByIndex(int(dev.Ifindex()))
		assert.NoError(t, err)
		assert.Equal(t, dev.Name(), iface.Name)
		assert.Equal(t, dev.MAC(), iface.HardwareAddr)
	})
}

func TestDevice_WritePacket(t *testing.T) {
	testsupport.VirtrunOnly(t)

	dev, tPacket := setupTestDevice(t)

	// Write a test packet to the TAP device.
	_, pkt := testsupport.TestPacket(t, dev.MAC(), 64)
	assert.NoError(t, dev.WritePacket(pkt))

	// Check if the packet arrived in the RAW socket.
	data, _, err := tPacket.ReadPacketData()
	assert.NoError(t, err)
	assert.Equal(t, pkt, data)
}

func TestDevice_ReadPacket(t *testing.T) {
	testsupport.VirtrunOnly(t)

	dev, tPacket := setupTestDevice(t)

	// Write a test packet to the RAW socket.
	_, pkt := testsupport.TestPacket(t, dev.MAC(), 64)
	assert.NoError(t, tPacket.WritePacketData(pkt))

	// Check if the packet arrived at the TAP device.
	receiveBuf := make([]byte, 1024)
	n, err := dev.ReadPacket(receiveBuf, time.Second)
	assert.NoError(t, err)
	assert.Equal(t, len(pkt), n)
	assert.Equal(t, pkt, receiveBuf[:n])
}

func TestDevice_ReadPacket_Timeout(t *testing.T) {
	testsupport.VirtrunOnly(t)

	dev, _ := setupTestDevice(t)

	// Try to receive a packet on the TAP device when none was sent.
	// This should time out.
	receiveBuf := make([]byte, 1024)
	_, err := dev.ReadPacket(receiveBuf, 500*time.Millisecond)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

func setupTestDevice(t *testing.T) (*tuntap.Device, *afpacket.TPacket) {
	t.Helper()
	testsupport.VirtrunOnly(t)

	// Make sure the Linux kernel does not send router solicitations that may
	// interfere with these tests.
	testsupport.SetSysctl(t, "net.ipv6.conf.all.disable_ipv6", "1")

	// Create a TAP device.
	dev, err := tuntap.NewDevice(
		tuntap.WithDeviceType(tuntap.DeviceTypeTAP),
		// Helps to stop the Linux kernel from sending packets on this
		// interface.
		tuntap.WithInterfaceFlags(unix.IFF_NOARP),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, dev.Close())
	})

	// Open a RAW socket to capture packets arriving at the TAP device or
	// write packets to it.
	tPacket, err := afpacket.NewTPacket(
		afpacket.SocketRaw,
		afpacket.TPacketVersion3,
		afpacket.OptInterface(dev.Name()),
	)
	require.NoError(t, err)
	t.Cleanup(tPacket.Close)

	return dev, tPacket
}
