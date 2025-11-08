package vhostnet

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/hetznercloud/virtio-go/vhost"
	"github.com/hetznercloud/virtio-go/virtio"
	"github.com/hetznercloud/virtio-go/virtqueue"
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

	receiveQueue  *virtqueue.SplitQueue
	transmitQueue *virtqueue.SplitQueue

	// transmitted contains channels for each possible descriptor chain head
	// index. This is used for packet transmit notifications.
	// When a packet was transmitted and the descriptor chain was used by the
	// device, the corresponding channel receives the [virtqueue.UsedElement]
	// instance provided by the device.
	transmitted []chan virtqueue.UsedElement
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
func NewDevice(options ...Option) (_ *Device, err error) {
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
	getFeatures, err := vhost.GetFeatures(dev.controlFD)
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
	if dev.receiveQueue, err = createQueue(dev.controlFD, receiveQueueIndex, opts.queueSize); err != nil {
		return nil, fmt.Errorf("create receive queue: %w", err)
	}
	if dev.transmitQueue, err = createQueue(dev.controlFD, transmitQueueIndex, opts.queueSize); err != nil {
		return nil, fmt.Errorf("create transmit queue: %w", err)
	}

	// Set up memory mappings for all buffers used by the queues. This has to
	// happen before a backend for the queues can be registered.
	memoryLayout := vhost.NewMemoryLayoutForQueues(
		[]*virtqueue.SplitQueue{dev.receiveQueue, dev.transmitQueue},
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

	// Initialize channels for transmit notifications.
	dev.transmitted = make([]chan virtqueue.UsedElement, dev.transmitQueue.Size())
	for i := range len(dev.transmitted) {
		// It is important to use a single-element buffered channel here.
		// When the channel was unbuffered and the monitorTransmitQueue
		// goroutine would write into it, the writing would block which could
		// lead to deadlocks in case transmit notifications do not arrive in
		// order.
		// When the goroutine would use fire-and-forget to write into that
		// channel, there may be a chance that the TransmitPacket does not
		// receive the transmit notification due to this being a race condition.
		// Buffering a single transmit notification resolves this without race
		// conditions or possible deadlocks.
		dev.transmitted[i] = make(chan virtqueue.UsedElement, 1)
	}

	// Monitor transmit queue in background.
	go dev.monitorTransmitQueue()

	dev.initialized = true

	// Make sure to clean up even when the device gets garbage collected without
	// Close being called first.
	devPtr := &dev
	runtime.SetFinalizer(devPtr, (*Device).Close)

	return devPtr, nil
}

// monitorTransmitQueue waits for the device to advertise used descriptor chains
// in the transmit queue and produces a transmit notification via the
// corresponding channel.
func (dev *Device) monitorTransmitQueue() {
	usedChan := dev.transmitQueue.UsedDescriptorChains()
	for {
		used, ok := <-usedChan
		if !ok {
			// The queue was closed.
			return
		}
		if int(used.DescriptorIndex) > len(dev.transmitted) {
			panic(fmt.Sprintf("device provided a used descriptor index (%d) that is out of range",
				used.DescriptorIndex))
		}

		dev.transmitted[used.DescriptorIndex] <- used
	}
}

// TransmitPacket writes the given packet into the transmit queue of this
// device. The packet will be prepended with the [virtio.NetHdr].
//
// When the queue is full, this will block until the queue has enough room to
// transmit the packet. This method will not return before the packet was
// transmitted and the device notifies that it has used the packet buffer.
func (dev *Device) TransmitPacket(vnethdr virtio.NetHdr, packet []byte) error {
	// Prepend the packet with its virtio-net header.
	vnethdrBuf := make([]byte, virtio.NetHdrSize+14) //todo WHY
	if err := vnethdr.Encode(vnethdrBuf); err != nil {
		return fmt.Errorf("encode vnethdr: %w", err)
	}
	vnethdrBuf[virtio.NetHdrSize+14-2] = 0x86
	vnethdrBuf[virtio.NetHdrSize+14-1] = 0xdd //todo ipv6 ethertype
	outBuffers := [][]byte{vnethdrBuf, packet}
	//outBuffers := [][]byte{packet}

	chainIndex, err := dev.transmitQueue.OfferDescriptorChain(outBuffers, 0, true)
	if err != nil {
		return fmt.Errorf("offer descriptor chain: %w", err)
	}

	// Wait for the packet to have been transmitted.
	<-dev.transmitted[chainIndex]

	if err = dev.transmitQueue.FreeDescriptorChain(chainIndex); err != nil {
		return fmt.Errorf("free descriptor chain: %w", err)
	}

	return nil
}

// ReceivePacket reads the next available packet from the receive queue of this
// device and returns its [virtio.NetHdr] and packet data separately.
//
// When no packet is available, this will block until there is one.
//
// When this method returns an error, the receive queue will likely be in a
// broken state which this implementation cannot recover from. The caller should
// close the device and not attempt any additional receives.
func (dev *Device) ReceivePacket() (virtio.NetHdr, []byte, error) {
	var (
		chainHeads []uint16

		vnethdr virtio.NetHdr
		buffers [][]byte

		// Each packet starts with a virtio-net header which we have to subtract
		// from the total length.
		packetLength = -virtio.NetHdrSize
	)

	// We presented FeatureNetMergeRXBuffers to the device, so one packet may be
	// made of multiple descriptor chains which are to be merged.
	for remainingChains := 1; remainingChains > 0; remainingChains-- {
		// Get the next descriptor chain.
		usedElement, ok := <-dev.receiveQueue.UsedDescriptorChains()
		if !ok {
			return virtio.NetHdr{}, nil, ErrDeviceClosed
		}

		// Track this chain to be freed later.
		head := uint16(usedElement.DescriptorIndex)
		chainHeads = append(chainHeads, head)

		outBuffers, inBuffers, err := dev.receiveQueue.GetDescriptorChain(head)
		if err != nil {
			// When this fails we may miss to free some descriptor chains. We
			// could try to mitigate this by deferring the freeing somehow, but
			// it's not worth the hassle. When this method fails, the queue will
			// be in a broken state anyway.
			return virtio.NetHdr{}, nil, fmt.Errorf("get descriptor chain: %w", err)
		}
		if len(outBuffers) > 0 {
			// How did this happen!?
			panic("receive queue contains device-readable buffers")
		}
		if len(inBuffers) == 0 {
			// Empty descriptor chains should not be possible.
			panic("descriptor chain contains no buffers")
		}

		// The device tells us how many bytes of the descriptor chain it has
		// actually written to. The specification forces the device to fully
		// fill up all but the last descriptor chain when multiple descriptor
		// chains are being merged, but being more compatible here doesn't hurt.
		inBuffers = truncateBuffers(inBuffers, int(usedElement.Length))
		packetLength += int(usedElement.Length)

		// Is this the first descriptor chain we process?
		if len(buffers) == 0 {
			// The specification requires that the first descriptor chain starts
			// with a virtio-net header. It is not clear, whether it is also
			// required to be fully contained in the first buffer of that
			// descriptor chain, but it is reasonable to assume that this is
			// always the case.
			// The decode method already does the buffer length check.
			if err = vnethdr.Decode(inBuffers[0]); err != nil {
				// The device misbehaved. There is no way we can gracefully
				// recover from this, because we don't know how many of the
				// following descriptor chains belong to this packet.
				return virtio.NetHdr{}, nil, fmt.Errorf("decode vnethdr: %w", err)
			}
			inBuffers[0] = inBuffers[0][virtio.NetHdrSize:]

			// The virtio-net header tells us how many descriptor chains this
			// packet is long.
			remainingChains = int(vnethdr.NumBuffers)
		}

		buffers = append(buffers, inBuffers...)
	}

	// Copy all the buffers together to produce the complete packet slice.
	packet := make([]byte, packetLength)
	copied := 0
	for _, buffer := range buffers {
		copied += copy(packet[copied:], buffer)
	}
	if copied != packetLength {
		panic(fmt.Sprintf("expected to copy %d bytes but only copied %d bytes", packetLength, copied))
	}

	// Now that we have copied all buffers, we can free the used descriptor
	// chains again.
	// TODO: Recycling the descriptor chains would be more efficient than
	//  freeing them just to offer them again right after.
	for _, head := range chainHeads {
		if err := dev.receiveQueue.FreeDescriptorChain(head); err != nil {
			return virtio.NetHdr{}, nil, fmt.Errorf("free descriptor chain with head index %d: %w", head, err)
		}
	}

	// It's advised to always keep the receive queue fully populated with
	// available buffers which the device can write new packets into.
	if err := dev.refillReceiveQueue(); err != nil {
		return virtio.NetHdr{}, nil, fmt.Errorf("refill receive queue: %w", err)
	}

	return vnethdr, packet, nil
}

// TODO: Make above methods cancelable by taking a context.Context argument?
// TODO: Implement zero-copy variants to transmit and receive packets?

// refillReceiveQueue offers as many new device-writable buffers to the device
// as the queue can fit. The device will then use these to write received
// packets.
func (dev *Device) refillReceiveQueue() error {
	for {
		_, err := dev.receiveQueue.OfferDescriptorChain(nil, 1, false)
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

	if dev.receiveQueue != nil {
		if err := dev.receiveQueue.Close(); err == nil {
			dev.receiveQueue = nil
		} else {
			errs = append(errs, fmt.Errorf("close receive queue: %w", err))
		}
	}

	if dev.transmitQueue != nil {
		if err := dev.transmitQueue.Close(); err == nil {
			dev.transmitQueue = nil
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
