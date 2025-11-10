package packet

type OutPacket struct {
	Segments [][]byte
	//todo virtio header?
	SegSize      int
	SegCounter   int
	Valid        bool
	wasSegmented bool

	Scratch []byte
}

func NewOut() *OutPacket {
	out := new(OutPacket)
	const numSegments = 64
	out.Segments = make([][]byte, numSegments)
	for i := 0; i < numSegments; i++ { //todo this is dumb
		out.Segments[i] = make([]byte, Size)
	}
	out.Scratch = make([]byte, Size)
	return out
}
