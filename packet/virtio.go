package packet

import (
	"github.com/slackhq/nebula/util/virtio"
)

type VirtIOPacket struct {
	Payload []byte
	buf     []byte
	Header  virtio.NetHdr
}

func NewVIO() *VirtIOPacket {
	out := new(VirtIOPacket)
	out.Payload = make([]byte, Size)
	out.buf = out.Payload
	return out
}

func (v *VirtIOPacket) Reset() {
	v.Payload = v.buf[:Size]
}
