package tuntap

import (
	"errors"

	"golang.org/x/sys/unix"
)

// DeviceType is the TUN/TAP device type.
type DeviceType int

const (
	// DeviceTypeTUN can be used to create TUN devices that operate on layer 3.
	// Packets that are transported over TUN devices do not have an Ethernet
	// header.
	DeviceTypeTUN DeviceType = unix.IFF_TUN
	// DeviceTypeTAP can be used to create TAP devices that operate on layer 2.
	// Packets that are transported over TAP devices do have an Ethernet header.
	DeviceTypeTAP DeviceType = unix.IFF_TAP
)

type optionValues struct {
	name           string
	deviceType     DeviceType
	virtioNetHdr   bool
	offloads       int
	interfaceFlags uint16
}

func (o *optionValues) apply(options []Option) {
	for _, option := range options {
		option(o)
	}
}

func (o *optionValues) validate() error {
	if len(o.name) >= unix.IFNAMSIZ {
		return errors.New("name must not be longer that 15 characters")
	}
	if o.deviceType != DeviceTypeTUN && o.deviceType != DeviceTypeTAP {
		return errors.New("device type is required and must be either TUN or TAP")
	}
	return nil
}

func (o *optionValues) ifreqFlags() uint16 {
	flags := uint16(o.deviceType)

	// Disable the packet information prefix.
	flags |= unix.IFF_NO_PI

	// Ensure the ioctl fails when a device with the same name already exists.
	flags |= unix.IFF_TUN_EXCL

	if o.virtioNetHdr {
		// Also requires the TUNSETVNETHDRSZ ioctl at a later time.
		flags |= unix.IFF_VNET_HDR
	}

	return flags
}

var optionDefaults = optionValues{
	// Let the kernel auto-select a name.
	name: "",
	// Required.
	deviceType: -1,
	// Don't enable it by default to avoid surprises.
	virtioNetHdr: false,
	// Optional. No offload support advertised by default.
	offloads: 0,
	// Optional. IFF_UP will always be set.
	interfaceFlags: 0,
}

// Option can be passed to [NewDevice] to influence device creation.
type Option func(*optionValues)

// WithName returns an [Option] that sets the name of the to be created device.
// This is optional. When no name is specified, the kernel will auto-select a
// name using the scheme "tunX" or "tapX".
func WithName(name string) Option {
	return func(o *optionValues) { o.name = name }
}

// WithDeviceType returns an [Option] that sets the type of device that should
// be created.
// This is required.
func WithDeviceType(deviceType DeviceType) Option {
	return func(o *optionValues) { o.deviceType = deviceType }
}

// WithVirtioNetHdr returns an [Option] that sets whether packets that are
// transported over the device are prepended with a [virtio.NetHdr].
// This is optional and disabled by default.
func WithVirtioNetHdr(enable bool) Option {
	return func(o *optionValues) { o.virtioNetHdr = enable }
}

// WithOffloads returns an [Option] that sets the supported offloads that the
// device should advertise. This tells the kernel which offloads the owner of
// the device can deal with ([unix.TUN_F_CSUM] for example).
// This is optional. By default, no offloads are supported.
// When configured, then [WithVirtioNetHdr] should also be enabled.
func WithOffloads(offloads int) Option {
	return func(o *optionValues) { o.offloads = offloads }
}

// WithInterfaceFlags returns an [Option] that sets the flags that should be
// used when taking the created interface up.
// This is optional. The [unix.IFF_UP] flag will always be set.
// The [unix.IFF_NOARP] flag may be useful in some scenarios to avoid packets
// from the Linux networking stack interfering with your application.
func WithInterfaceFlags(flags uint16) Option {
	return func(o *optionValues) { o.interfaceFlags = flags }
}
