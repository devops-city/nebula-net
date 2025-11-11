package packet

import (
	"github.com/slackhq/nebula/util/virtio"
)

type VirtIOPacket struct {
	Payload []byte
	Header  virtio.NetHdr
}

func NewVIO() *VirtIOPacket {
	out := new(VirtIOPacket)
	out.Payload = make([]byte, Size)
	return out
}
