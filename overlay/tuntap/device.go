package tuntap

import (
	"fmt"
	"net"
	"os"
	"time"
	"unsafe"

	"github.com/hetznercloud/virtio-go/virtio"
	"golang.org/x/sys/unix"
)

// Documentation:
//   https://docs.kernel.org/networking/tuntap.html
// Also worth a read:
//   https://blog.cloudflare.com/virtual-networking-101-understanding-tap/

// Device represents a TUN/TAP device.
type Device struct {
	name    string
	ifindex uint32
	mac     net.HardwareAddr
	file    *os.File
}

// NewDevice creates a new TUN/TAP device, brings it up, and returns a [Device]
// instance providing access to it.
//
// There are multiple options that can be passed to this constructor to
// influence device creation:
//   - [WithName]
//   - [WithDeviceType]
//   - [WithVirtioNetHdr]
//   - [WithInterfaceFlags]
//
// Remember to call [Device.Close] after use to free up resources.
func NewDevice(options ...Option) (_ *Device, err error) {
	opts := optionDefaults
	opts.apply(options)
	if err = opts.validate(); err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}

	// Get a file descriptor. The device will exist as long as we keep this
	// file descriptor open.
	fd, err := unix.Open("/dev/net/tun", os.O_RDWR, 0666)
	if err != nil {
		return nil, fmt.Errorf("access tuntap driver: %w", err)
	}

	// Create an interface request. When the name is empty, the kernel will
	// auto-select one.
	ifreq, err := unix.NewIfreq(opts.name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("new ifreq: %w", err)
	}

	// Create the new device.
	ifreq.SetUint16(opts.ifreqFlags())
	if err = unix.IoctlIfreq(fd, unix.TUNSETIFF, ifreq); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create device: %w", err)
	}

	dev := Device{
		// The TUNSETIFF ioctl writes the actual name that was chosen for the
		// device back to the request, so use that.
		name: ifreq.Name(),
	}

	// Make the file descriptor of the device non-blocking. This enables us to
	// cancel reads after a timeout when no packets are arriving.
	// This, and the call to NewFile has to happen after creating the device:
	// https://github.com/golang/go/issues/30426#issuecomment-470330742
	// NewFile will recognize that the file descriptor is non-blocking and will
	// configure polling for it.
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("make file descriptor non-blocking: %w", err)
	}

	// By wrapping the file descriptor as an os.File, we not only have a
	// convenient way to read and write, but also register a finalizer that
	// closes the file descriptor when it's being garbage collected.
	dev.file = os.NewFile(uintptr(fd), dev.name)

	// Make sure the device is removed when one of the following initialization
	// steps fails.
	defer func() {
		if err != nil {
			_ = dev.Close()
		}
	}()

	if opts.virtioNetHdr {
		// Tell the device which size we use for our virtio_net_hdr.
		err = unix.IoctlSetPointerInt(fd, unix.TUNSETVNETHDRSZ, virtio.NetHdrSize)
		if err != nil {
			return nil, fmt.Errorf("set vnethdr size: %w", err)
		}
	}

	// Tell the device which offloads are supported.
	err = unix.IoctlSetInt(fd, unix.TUNSETOFFLOAD, opts.offloads)
	if err != nil {
		return nil, fmt.Errorf("set offloads: %w", err)
	}

	// For the following ioctls we need just any AF_INET socket, so create one.
	inet, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("open inet socket: %w", err)
	}
	defer func() { _ = unix.Close(inet) }()

	// Set the interface flags to bring it up.
	ifreq.SetUint16(unix.IFF_UP | opts.interfaceFlags)
	if err = unix.IoctlIfreq(inet, unix.SIOCSIFFLAGS, ifreq); err != nil {
		return nil, fmt.Errorf("set interface flags: %w", err)
	}

	// Get the interface index.
	if err = unix.IoctlIfreq(inet, unix.SIOCGIFINDEX, ifreq); err != nil {
		return nil, fmt.Errorf("get interface index: %w", err)
	}
	dev.ifindex = ifreq.Uint32()

	// Get the MAC address.
	// This ioctl writes a sockaddr into the data ifru section of the interface
	// request struct. The MAC address is in the beginning of the
	// sockaddr.sa_data section.
	if err = unix.IoctlIfreq(inet, unix.SIOCGIFHWADDR, ifreq); err != nil {
		return nil, fmt.Errorf("get mac address: %w", err)
	}
	dev.mac = unsafe.Slice((*byte)(unsafe.Pointer(ifreq)), 32)[16+2 : 16+8]

	return &dev, nil
}

// Close closes the file descriptor behind this device. This will cause the
// TUN/TAP device to be removed.
func (dev *Device) Close() error {
	if err := dev.file.Close(); err != nil {
		return fmt.Errorf("close file descriptor: %w", err)
	}
	dev.file = nil
	return nil
}

// Name returns the name of this device.
func (dev *Device) Name() string {
	dev.ensureInitialized()
	return dev.name
}

// Ifindex returns the interface index of this device.
func (dev *Device) Ifindex() uint32 {
	dev.ensureInitialized()
	return dev.ifindex
}

// MAC returns the hardware address of this device.
func (dev *Device) MAC() net.HardwareAddr {
	dev.ensureInitialized()
	return dev.mac
}

// File returns the [os.File] that is used to communicate with this device.
// If you access it directly, please be careful to not interfere with this
// implementation.
func (dev *Device) File() *os.File {
	dev.ensureInitialized()
	return dev.file
}

// WritePacket writes the given packet to the TUN/TAP device.
// When the [WithVirtioNetHdr] option was enabled, then the caller is
// responsible to prepend the packet with a [virtio.NetHdr].
func (dev *Device) WritePacket(packet []byte) error {
	dev.ensureInitialized()

	_, err := dev.file.Write(packet)
	if err != nil {
		return fmt.Errorf("write %d bytes: %w", len(packet), err)
	}

	return nil
}

// ReadPacket reads the next available packet from the TUN/TAP device into the
// given buffer. Make sure that the buffer is large enough, otherwise only a
// part of the packet may be read. The number of read bytes will be returned.
//
// When the [WithVirtioNetHdr] option was enabled, then the read packet will be
// prepended with a [virtio.NetHdr]. The caller is responsible to handle it
// accordingly.
//
// A timeout can be given to limit the time this operation blocks. If no packet
// arrives within the given timeout, the read is canceled and an error that
// wraps [os.ErrDeadlineExceeded] is returned. Pass a timeout of zero to make
// this operation block infinitely.
func (dev *Device) ReadPacket(buf []byte, timeout time.Duration) (int, error) {
	dev.ensureInitialized()

	// Make sure the read times out. This only works for files that support
	// polling (see above).
	// When no timeout is desired, passing the zero time removes the deadline.
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if err := dev.file.SetReadDeadline(deadline); err != nil {
		return 0, fmt.Errorf("set deadline: %w", err)
	}

	n, err := dev.file.Read(buf)
	if err != nil {
		return n, fmt.Errorf("read up to %d bytes: %w", len(buf), err)
	}

	return n, nil
}

// ensureInitialized is used as a guard to prevent methods to be called on an
// uninitialized instance.
func (dev *Device) ensureInitialized() {
	if dev.file == nil {
		panic("device is not initialized")
	}
}
