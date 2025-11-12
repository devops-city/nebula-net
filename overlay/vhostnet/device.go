package vhostnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"

	"github.com/slackhq/nebula/overlay/vhost"
	"github.com/slackhq/nebula/overlay/virtqueue"
	"github.com/slackhq/nebula/packet"
	"github.com/slackhq/nebula/util/virtio"
	"golang.org/x/sys/unix"
)

// ErrDeviceClosed is returned when the [Device] is closed while operations are
// still running.
var ErrDeviceClosed = errors.New("device was closed")

// The indexes for the receive and transmit queues.
const (
	receiveQueueIndex  = 0
	transmitQueueIndex = 1
)

// Device represents a vhost networking device within the kernel-level virtio
// implementation and provides methods to interact with it.
type Device struct {
	initialized bool
	controlFD   int

	ReceiveQueue  *virtqueue.SplitQueue
	TransmitQueue *virtqueue.SplitQueue
}

// NewDevice initializes a new vhost networking device within the
// kernel-level virtio implementation, sets up the virtqueues and returns a
// [Device] instance that can be used to communicate with that vhost device.
//
// There are multiple options that can be passed to this constructor to
// influence device creation:
//   - [WithQueueSize]
//   - [WithBackendFD]
//   - [WithBackendDevice]
//
// Remember to call [Device.Close] after use to free up resources.
func NewDevice(options ...Option) (*Device, error) {
	var err error
	opts := optionDefaults
	opts.apply(options)
	if err = opts.validate(); err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}

	dev := Device{
		controlFD: -1,
	}

	// Clean up a partially initialized device when something fails.
	defer func() {
		if err != nil {
			_ = dev.Close()
		}
	}()

	// Retrieve a new control file descriptor. This will be used to configure
	// the vhost networking device in the kernel.
	dev.controlFD, err = unix.Open("/dev/vhost-net", os.O_RDWR, 0666)
	if err != nil {
		return nil, fmt.Errorf("get control file descriptor: %w", err)
	}
	if err = vhost.OwnControlFD(dev.controlFD); err != nil {
		return nil, fmt.Errorf("own control file descriptor: %w", err)
	}

	// Advertise the supported features. This isn't much for now.
	// TODO: Add feature options and implement proper feature negotiation.
	getFeatures, err := vhost.GetFeatures(dev.controlFD) //0x1033D008000 but why
	if err != nil {
		return nil, fmt.Errorf("get features: %w", err)
	}
	if getFeatures == 0 {

	}
	//const funky = virtio.Feature(1 << 27)
	//features := virtio.FeatureVersion1 | funky // | todo virtio.FeatureNetMergeRXBuffers
	features := virtio.FeatureVersion1 | virtio.FeatureNetMergeRXBuffers
	if err = vhost.SetFeatures(dev.controlFD, features); err != nil {
		return nil, fmt.Errorf("set features: %w", err)
	}

	// Initialize and register the queues needed for the networking device.
	if dev.ReceiveQueue, err = createQueue(dev.controlFD, receiveQueueIndex, opts.queueSize); err != nil {
		return nil, fmt.Errorf("create receive queue: %w", err)
	}
	if dev.TransmitQueue, err = createQueue(dev.controlFD, transmitQueueIndex, opts.queueSize); err != nil {
		return nil, fmt.Errorf("create transmit queue: %w", err)
	}

	// Set up memory mappings for all buffers used by the queues. This has to
	// happen before a backend for the queues can be registered.
	memoryLayout := vhost.NewMemoryLayoutForQueues(
		[]*virtqueue.SplitQueue{dev.ReceiveQueue, dev.TransmitQueue},
	)
	if err = vhost.SetMemoryLayout(dev.controlFD, memoryLayout); err != nil {
		return nil, fmt.Errorf("setup memory layout: %w", err)
	}

	// Set the queue backends. This activates the queues within the kernel.
	if err = SetQueueBackend(dev.controlFD, receiveQueueIndex, opts.backendFD); err != nil {
		return nil, fmt.Errorf("set receive queue backend: %w", err)
	}
	if err = SetQueueBackend(dev.controlFD, transmitQueueIndex, opts.backendFD); err != nil {
		return nil, fmt.Errorf("set transmit queue backend: %w", err)
	}

	// Fully populate the receive queue with available buffers which the device
	// can write new packets into.
	if err = dev.refillReceiveQueue(); err != nil {
		return nil, fmt.Errorf("refill receive queue: %w", err)
	}

	dev.initialized = true

	// Make sure to clean up even when the device gets garbage collected without
	// Close being called first.
	devPtr := &dev
	runtime.SetFinalizer(devPtr, (*Device).Close)

	return devPtr, nil
}

// refillReceiveQueue offers as many new device-writable buffers to the device
// as the queue can fit. The device will then use these to write received
// packets.
func (dev *Device) refillReceiveQueue() error {
	for {
		_, err := dev.ReceiveQueue.OfferInDescriptorChains(1)
		if err != nil {
			if errors.Is(err, virtqueue.ErrNotEnoughFreeDescriptors) {
				// Queue is full, job is done.
				return nil
			}
			return fmt.Errorf("offer descriptor chain: %w", err)
		}
	}
}

// Close cleans up the vhost networking device within the kernel and releases
// all resources used for it.
// The implementation will try to release as many resources as possible and
// collect potential errors before returning them.
func (dev *Device) Close() error {
	dev.initialized = false

	// Closing the control file descriptor will unregister all queues from the
	// kernel.
	if dev.controlFD >= 0 {
		if err := unix.Close(dev.controlFD); err != nil {
			// Return an error and do not continue, because the memory used for
			// the queues should not be released before they were unregistered
			// from the kernel.
			return fmt.Errorf("close control file descriptor: %w", err)
		}
		dev.controlFD = -1
	}

	var errs []error

	if dev.ReceiveQueue != nil {
		if err := dev.ReceiveQueue.Close(); err == nil {
			dev.ReceiveQueue = nil
		} else {
			errs = append(errs, fmt.Errorf("close receive queue: %w", err))
		}
	}

	if dev.TransmitQueue != nil {
		if err := dev.TransmitQueue.Close(); err == nil {
			dev.TransmitQueue = nil
		} else {
			errs = append(errs, fmt.Errorf("close transmit queue: %w", err))
		}
	}

	if len(errs) == 0 {
		// Everything was cleaned up. No need to run the finalizer anymore.
		runtime.SetFinalizer(dev, nil)
	}

	return errors.Join(errs...)
}

// ensureInitialized is used as a guard to prevent methods to be called on an
// uninitialized instance.
func (dev *Device) ensureInitialized() {
	if !dev.initialized {
		panic("device is not initialized")
	}
}

// createQueue creates a new virtqueue and registers it with the vhost device
// using the given index.
func createQueue(controlFD int, queueIndex int, queueSize int) (*virtqueue.SplitQueue, error) {
	var (
		queue *virtqueue.SplitQueue
		err   error
	)
	if queue, err = virtqueue.NewSplitQueue(queueSize); err != nil {
		return nil, fmt.Errorf("create virtqueue: %w", err)
	}
	if err = vhost.RegisterQueue(controlFD, uint32(queueIndex), queue); err != nil {
		return nil, fmt.Errorf("register virtqueue with index %d: %w", queueIndex, err)
	}
	return queue, nil
}

// truncateBuffers returns a new list of buffers whose combined length matches
// exactly the specified length. When the specified length exceeds the length of
// the buffers, this is an error. When it is smaller, the buffer list will be
// truncated accordingly.
func truncateBuffers(buffers [][]byte, length int) (out [][]byte) {
	for _, buffer := range buffers {
		if length < len(buffer) {
			out = append(out, buffer[:length])
			return
		}
		out = append(out, buffer)
		length -= len(buffer)
	}
	if length > 0 {
		panic("length exceeds the combined length of all buffers")
	}
	return
}

func (dev *Device) TransmitPackets(vnethdr virtio.NetHdr, packets [][]byte) error {
	// Prepend the packet with its virtio-net header.
	vnethdrBuf := make([]byte, virtio.NetHdrSize+14) //todo WHY
	if err := vnethdr.Encode(vnethdrBuf); err != nil {
		return fmt.Errorf("encode vnethdr: %w", err)
	}
	vnethdrBuf[virtio.NetHdrSize+14-2] = 0x86
	vnethdrBuf[virtio.NetHdrSize+14-1] = 0xdd //todo ipv6 ethertype

	chainIndexes, err := dev.TransmitQueue.OfferOutDescriptorChains(vnethdrBuf, packets)
	if err != nil {
		return fmt.Errorf("offer descriptor chain: %w", err)
	}
	//todo surely there's something better to do here

	for {
		txedChains, err := dev.TransmitQueue.BlockAndGetHeadsCapped(context.TODO(), len(chainIndexes))
		if err != nil {
			return err
		} else if len(txedChains) == 0 {
			continue //todo will this ever exit?
		}
		for _, c := range txedChains {
			idx := slices.Index(chainIndexes, c.GetHead())
			if idx < 0 {
				continue
			} else {
				_ = dev.TransmitQueue.FreeDescriptorChain(chainIndexes[idx])
				chainIndexes[idx] = 0 //todo I hope this works
			}
		}
		done := true //optimism!
		for _, x := range chainIndexes {
			if x != 0 {
				done = false
				break
			}
		}

		if done {
			return nil
		}
	}
}

// TODO: Make above methods cancelable by taking a context.Context argument?
// TODO: Implement zero-copy variants to transmit and receive packets?

// processChains processes as many chains as needed to create one packet. The number of processed chains is returned.
func (dev *Device) processChains(pkt *packet.VirtIOPacket, chains []virtqueue.UsedElement) (int, error) {
	//read first element to see how many descriptors we need:
	pkt.Reset()
	n, err := dev.ReceiveQueue.GetDescriptorChainContents(uint16(chains[0].DescriptorIndex), pkt.Payload, int(chains[0].Length)) //todo
	if err != nil {
		return 0, err
	}
	// The specification requires that the first descriptor chain starts
	// with a virtio-net header. It is not clear, whether it is also
	// required to be fully contained in the first buffer of that
	// descriptor chain, but it is reasonable to assume that this is
	// always the case.
	// The decode method already does the buffer length check.
	if err = pkt.Header.Decode(pkt.Payload[0:]); err != nil {
		// The device misbehaved. There is no way we can gracefully
		// recover from this, because we don't know how many of the
		// following descriptor chains belong to this packet.
		return 0, fmt.Errorf("decode vnethdr: %w", err)
	}

	//we have the header now: what do we need to do?
	if int(pkt.Header.NumBuffers) > len(chains) {
		return 0, fmt.Errorf("number of buffers is greater than number of chains %d", len(chains))
	}

	//shift the buffer out of out:
	pkt.Payload = pkt.Payload[virtio.NetHdrSize:]

	cursor := n - virtio.NetHdrSize

	if uint32(n) >= chains[0].Length && pkt.Header.NumBuffers == 1 {
		pkt.Payload = pkt.Payload[:chains[0].Length-virtio.NetHdrSize]
		return 1, nil
	}

	i := 1
	// we used chain 0 already
	for i = 1; i < len(chains); i++ {
		n, err = dev.ReceiveQueue.GetDescriptorChainContents(uint16(chains[i].DescriptorIndex), pkt.Payload[cursor:], int(chains[i].Length))
		if err != nil {
			// When this fails we may miss to free some descriptor chains. We
			// could try to mitigate this by deferring the freeing somehow, but
			// it's not worth the hassle. When this method fails, the queue will
			// be in a broken state anyway.
			return i, fmt.Errorf("get descriptor chain: %w", err)
		}
		cursor += n
	}
	//todo this has to be wrong
	pkt.Payload = pkt.Payload[:cursor]
	return i, nil
}

func (dev *Device) ReceivePackets(out []*packet.VirtIOPacket) (int, error) {
	//todo optimize?
	var chains []virtqueue.UsedElement
	var err error
	//if len(dev.extraRx) == 0 {
	chains, err = dev.ReceiveQueue.BlockAndGetHeadsCapped(context.TODO(), 64) //todo config batch
	if err != nil {
		return 0, err
	}
	if len(chains) == 0 {
		return 0, nil
	}
	//} else {
	//	chains = dev.extraRx
	//}

	numPackets := 0
	chainsIdx := 0
	for numPackets = 0; chainsIdx < len(chains); numPackets++ {
		if numPackets >= len(out) {
			return numPackets, fmt.Errorf("dropping %d packets, no room", len(chains)-numPackets)
		}
		numChains, err := dev.processChains(out[numPackets], chains[chainsIdx:])
		if err != nil {
			return 0, err
		}
		chainsIdx += numChains
	}

	// Now that we have copied all buffers, we can recycle the used descriptor chains
	if err = dev.ReceiveQueue.RecycleDescriptorChains(chains); err != nil {
		return 0, err
	}

	return numPackets, nil
}
