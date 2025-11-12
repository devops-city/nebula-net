package packet

import (
	"github.com/slackhq/nebula/util/virtio"
)

type VirtIOPacket struct {
	Payload   []byte
	Header    virtio.NetHdr
	Chains    []uint16
	ChainRefs [][]byte
	// RecycleDescriptorChains(chains []uint16, kick bool) error
	Recycler func([]uint16, bool) error
}

func NewVIO() *VirtIOPacket {
	out := new(VirtIOPacket)
	out.Payload = make([]byte, Size)
	out.ChainRefs = make([][]byte, 0, 4)
	out.Chains = make([]uint16, 0, 8)
	return out
}

func (v *VirtIOPacket) Reset() {
	v.Payload = nil
	v.ChainRefs = v.ChainRefs[:0]
	v.Chains = v.Chains[:0]
}

func (v *VirtIOPacket) Recycle(lastOne bool) error {
	if v.Recycler != nil {
		err := v.Recycler(v.Chains, lastOne)
		if err != nil {
			return err
		}
	}
	v.Reset()
	return nil
}
