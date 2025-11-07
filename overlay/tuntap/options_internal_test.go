package tuntap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestOptionValues_Apply(t *testing.T) {
	opts := optionDefaults
	opts.apply([]Option{
		WithName("name"),
		WithDeviceType(DeviceTypeTAP),
		WithVirtioNetHdr(true),
		WithOffloads(unix.TUN_F_CSUM),
		WithInterfaceFlags(unix.IFF_NOARP),
	})

	assert.Equal(t, optionValues{
		name:           "name",
		deviceType:     DeviceTypeTAP,
		virtioNetHdr:   true,
		offloads:       unix.TUN_F_CSUM,
		interfaceFlags: unix.IFF_NOARP,
	}, opts)
}

func TestOptionValues_Validate(t *testing.T) {
	tests := []struct {
		name      string
		values    optionValues
		assertErr assert.ErrorAssertionFunc
	}{
		{
			name: "name too long",
			values: optionValues{
				name:       "thisisaverylongname",
				deviceType: DeviceTypeTAP,
			},
			assertErr: assert.Error,
		},
		{
			name:      "device type missing",
			values:    optionValues{},
			assertErr: assert.Error,
		},
		{
			name: "invalid device type",
			values: optionValues{
				deviceType: 999,
			},
			assertErr: assert.Error,
		},
		{
			name: "valid minimal",
			values: optionValues{
				deviceType: DeviceTypeTAP,
			},
			assertErr: assert.NoError,
		},
		{
			name: "valid full",
			values: optionValues{
				name:           "name",
				deviceType:     DeviceTypeTAP,
				virtioNetHdr:   true,
				offloads:       unix.TUN_F_CSUM,
				interfaceFlags: unix.IFF_NOARP,
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
