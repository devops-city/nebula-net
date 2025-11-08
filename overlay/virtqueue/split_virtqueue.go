package virtqueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/slackhq/nebula/overlay/eventfd"
	"golang.org/x/sys/unix"
)

// SplitQueue is a virtqueue that consists of several parts, where each part is
// writeable by either the driver or the device, but not both.
type SplitQueue struct {
	// size is the size of the queue.
	size int
	// buf is the underlying memory used for the queue.
	buf []byte

	descriptorTable *DescriptorTable
	availableRing   *AvailableRing
	usedRing        *UsedRing

	// kickEventFD is used to signal the device when descriptor chains were
	// added to the available ring.
	kickEventFD eventfd.EventFD
	// callEventFD is used by the device to signal when it has used descriptor
	// chains and put them in the used ring.
	callEventFD eventfd.EventFD

	// usedChains is a chanel that receives [UsedElement]s for descriptor chains
	// that were used by the device.
	usedChains chan UsedElement

	// moreFreeDescriptors is a channel that signals when any descriptors were
	// put back into the free chain of the descriptor table. This is used to
	// unblock methods waiting for available room in the queue to create new
	// descriptor chains again.
	moreFreeDescriptors chan struct{}

	// stop is used by [SplitQueue.Close] to cancel the goroutine that handles
	// used buffer notifications. It blocks until the goroutine ended.
	stop func() error

	// offerMutex is used to synchronize calls to
	// [SplitQueue.OfferDescriptorChain].
	offerMutex sync.Mutex
	pageSize   int
	itemSize   int
}

// NewSplitQueue allocates a new [SplitQueue] in memory. The given queue size
// specifies the number of entries/buffers the queue can hold. This also affects
// the memory consumption.
func NewSplitQueue(queueSize int) (_ *SplitQueue, err error) {
	if err = CheckQueueSize(queueSize); err != nil {
		return nil, err
	}

	sq := SplitQueue{
		size:     queueSize,
		pageSize: os.Getpagesize(),
		itemSize: os.Getpagesize(), //todo config
	}

	// Clean up a partially initialized queue when something fails.
	defer func() {
		if err != nil {
			_ = sq.Close()
		}
	}()

	// There are multiple ways for how the memory for the virtqueue could be
	// allocated. We could use Go native structs with arrays inside them, but
	// this wouldn't allow us to make the queue size configurable. And including
	// a slice in the Go structs wouldn't work, because this would just put the
	// Go slice descriptor into the memory region which the virtio device will
	// not understand.
	// Additionally, Go does not allow us to ensure a correct alignment of the
	// parts of the virtqueue, as it is required by the virtio specification.
	//
	// To resolve this, let's just allocate the memory manually by allocating
	// one or more memory pages, depending on the queue size. Making the
	// virtqueue start at the beginning of a page is not strictly necessary, as
	// the virtio specification does not require it to be continuous in the
	// physical memory of the host (e.g. the vhost implementation in the kernel
	// always uses copy_from_user to access it), but this makes it very easy to
	// guarantee the alignment. Also, it is not required for the virtqueue parts
	// to be in the same memory region, as we pass separate pointers to them to
	// the device, but this design just makes things easier to implement.
	//
	// One added benefit of allocating the memory manually is, that we have full
	// control over its lifetime and don't risk the garbage collector to collect
	// our valuable structures while the device still works with them.

	// The descriptor table is at the start of the page, so alignment is not an
	// issue here.
	descriptorTableStart := 0
	descriptorTableEnd := descriptorTableStart + descriptorTableSize(queueSize)
	availableRingStart := align(descriptorTableEnd, availableRingAlignment)
	availableRingEnd := availableRingStart + availableRingSize(queueSize)
	usedRingStart := align(availableRingEnd, usedRingAlignment)
	usedRingEnd := usedRingStart + usedRingSize(queueSize)

	sq.buf, err = unix.Mmap(-1, 0, usedRingEnd,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, fmt.Errorf("allocate virtqueue buffer: %w", err)
	}

	sq.descriptorTable = newDescriptorTable(queueSize, sq.buf[descriptorTableStart:descriptorTableEnd], sq.itemSize)
	sq.availableRing = newAvailableRing(queueSize, sq.buf[availableRingStart:availableRingEnd])
	sq.usedRing = newUsedRing(queueSize, sq.buf[usedRingStart:usedRingEnd])

	sq.kickEventFD, err = eventfd.New()
	if err != nil {
		return nil, fmt.Errorf("create kick event file descriptor: %w", err)
	}
	sq.callEventFD, err = eventfd.New()
	if err != nil {
		return nil, fmt.Errorf("create call event file descriptor: %w", err)
	}

	if err = sq.descriptorTable.initializeDescriptors(); err != nil {
		return nil, fmt.Errorf("initialize descriptors: %w", err)
	}

	// Initialize channels.
	sq.usedChains = make(chan UsedElement, queueSize)
	sq.moreFreeDescriptors = make(chan struct{})

	// Consume used buffer notifications in the background.
	sq.stop = sq.startConsumeUsedRing()

	return &sq, nil
}

// Size returns the size of this queue, which is the number of entries/buffers
// this queue can hold.
func (sq *SplitQueue) Size() int {
	sq.ensureInitialized()
	return sq.size
}

// DescriptorTable returns the [DescriptorTable] behind this queue.
func (sq *SplitQueue) DescriptorTable() *DescriptorTable {
	sq.ensureInitialized()
	return sq.descriptorTable
}

// AvailableRing returns the [AvailableRing] behind this queue.
func (sq *SplitQueue) AvailableRing() *AvailableRing {
	sq.ensureInitialized()
	return sq.availableRing
}

// UsedRing returns the [UsedRing] behind this queue.
func (sq *SplitQueue) UsedRing() *UsedRing {
	sq.ensureInitialized()
	return sq.usedRing
}

// KickEventFD returns the kick event file descriptor behind this queue.
// The returned file descriptor should be used with great care to not interfere
// with this implementation.
func (sq *SplitQueue) KickEventFD() int {
	sq.ensureInitialized()
	return sq.kickEventFD.FD()
}

// CallEventFD returns the call event file descriptor behind this queue.
// The returned file descriptor should be used with great care to not interfere
// with this implementation.
func (sq *SplitQueue) CallEventFD() int {
	sq.ensureInitialized()
	return sq.callEventFD.FD()
}

// UsedDescriptorChains returns the channel that receives [UsedElement]s for all
// descriptor chains that were used by the device.
//
// Users of the [SplitQueue] should read from this channel, handle the used
// descriptor chains and free them using [SplitQueue.FreeDescriptorChain] when
// they're done with them. When this does not happen, the queue will run full
// and any further calls to [SplitQueue.OfferDescriptorChain] will stall.
//
// When [SplitQueue.Close] is called, this channel will be closed as well.
func (sq *SplitQueue) UsedDescriptorChains() chan UsedElement {
	sq.ensureInitialized()
	return sq.usedChains
}

// startConsumeUsedRing starts a goroutine that runs [consumeUsedRing].
// A function is returned that can be used to gracefully cancel it.
func (sq *SplitQueue) startConsumeUsedRing() func() error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)

	ep, err := eventfd.NewEpoll()
	if err != nil {
		panic(err)
	}
	err = ep.AddEvent(sq.callEventFD.FD())
	if err != nil {
		panic(err)
	}

	go func() {
		done <- sq.consumeUsedRing(ctx, &ep)
	}()
	return func() error {
		cancel()

		// The goroutine blocks until it receives a signal on the event file
		// descriptor, so it will never notice the context being canceled.
		// To resolve this, we can just produce a fake-signal ourselves to wake
		// it up.
		if err := sq.callEventFD.Kick(); err != nil {
			return fmt.Errorf("wake up goroutine: %w", err)
		}

		// Wait for the goroutine to end. This prevents the event file
		// descriptor to be closed while it's still being used.
		// If the goroutine failed, this is the last chance to propagate the
		// error so it at least doesn't go unnoticed, even though the error may
		// be older already.
		if err := <-done; err != nil {
			return fmt.Errorf("goroutine: consume used ring: %w", err)
		}
		return nil
	}
}

// consumeUsedRing runs in a goroutine, waits for the device to signal that it
// has used descriptor chains and puts all new [UsedElement]s into the channel
// for them.
func (sq *SplitQueue) consumeUsedRing(ctx context.Context, epoll *eventfd.Epoll) error {
	var n int
	var err error
	for ctx.Err() == nil {

		// Wait for a signal from the device.
		if n, err = epoll.Block(); err != nil {
			return fmt.Errorf("wait: %w", err)
		}

		if n > 0 {
			_ = epoll.Clear() //???

			// Process all new used elements.
			for _, usedElement := range sq.usedRing.take() {
				sq.usedChains <- usedElement
			}
		}
	}

	return nil
}

// blockForMoreDescriptors blocks on a channel waiting for more descriptors to free up.
// it is its own function so maybe it might show up in pprof
func (sq *SplitQueue) blockForMoreDescriptors() {
	<-sq.moreFreeDescriptors
}

// OfferDescriptorChain offers a descriptor chain to the device which contains a
// number of device-readable buffers (out buffers) and device-writable buffers
// (in buffers).
//
// All buffers in the outBuffers slice will be concatenated by chaining
// descriptors, one for each buffer in the slice. When a buffer is too large to
// fit into a single descriptor (limited by the system's page size), it will be
// split up into multiple descriptors within the chain.
// When numInBuffers is greater than zero, the given number of device-writable
// descriptors will be appended to the end of the chain, each referencing a
// whole memory page (see [os.Getpagesize]).
//
// When the queue is full and no more descriptor chains can be added, a wrapped
// [ErrNotEnoughFreeDescriptors] will be returned. If you set waitFree to true,
// this method will handle this error and will block instead until there are
// enough free descriptors again.
//
// After defining the descriptor chain in the [DescriptorTable], the index of
// the head of the chain will be made available to the device using the
// [AvailableRing] and will be returned by this method.
// Callers should read from the [SplitQueue.UsedDescriptorChains] channel to be
// notified when the descriptor chain was used by the device and should free the
// used descriptor chains again using [SplitQueue.FreeDescriptorChain] when
// they're done with them. When this does not happen, the queue will run full
// and any further calls to [SplitQueue.OfferDescriptorChain] will stall.
func (sq *SplitQueue) OfferDescriptorChain(outBuffers [][]byte, numInBuffers int, waitFree bool) (uint16, error) {
	sq.ensureInitialized()

	// TODO change this
	// Each descriptor can only hold a whole memory page, so split large out
	// buffers into multiple smaller ones.
	outBuffers = splitBuffers(outBuffers, sq.pageSize)

	// Synchronize the offering of descriptor chains. While the descriptor table
	// and available ring are synchronized on their own as well, this does not
	// protect us from interleaved calls which could cause reordering.
	// By locking here, we can ensure that all descriptor chains are made
	// available to the device in the same order as this method was called.
	sq.offerMutex.Lock()
	defer sq.offerMutex.Unlock()

	// Create a descriptor chain for the given buffers.
	var (
		head uint16
		err  error
	)
	for {
		head, err = sq.descriptorTable.createDescriptorChain(outBuffers, numInBuffers)
		if err == nil {
			break
		}

		// I don't wanna use errors.Is, it's slow
		//goland:noinspection GoDirectComparisonOfErrors
		if err == ErrNotEnoughFreeDescriptors {
			if waitFree {
				// Wait for more free descriptors to be put back into the queue.
				// If the number of free descriptors is still not sufficient, we'll
				// land here again.
				sq.blockForMoreDescriptors()
				continue
			} else {
				return 0, err
			}
		}
		return 0, fmt.Errorf("create descriptor chain: %w", err)
	}

	// Make the descriptor chain available to the device.
	sq.availableRing.offer([]uint16{head})

	// Notify the device to make it process the updated available ring.
	if err := sq.kickEventFD.Kick(); err != nil {
		return head, fmt.Errorf("notify device: %w", err)
	}

	return head, nil
}

func (sq *SplitQueue) OfferOutDescriptorChains(prepend []byte, outBuffers [][]byte, waitFree bool) ([]uint16, error) {
	sq.ensureInitialized()

	// TODO change this
	// Each descriptor can only hold a whole memory page, so split large out
	// buffers into multiple smaller ones.
	outBuffers = splitBuffers(outBuffers, sq.pageSize)

	// Synchronize the offering of descriptor chains. While the descriptor table
	// and available ring are synchronized on their own as well, this does not
	// protect us from interleaved calls which could cause reordering.
	// By locking here, we can ensure that all descriptor chains are made
	// available to the device in the same order as this method was called.
	sq.offerMutex.Lock()
	defer sq.offerMutex.Unlock()

	chains := make([]uint16, len(outBuffers))

	// Create a descriptor chain for the given buffers.
	var (
		head uint16
		err  error
	)
	for i := range outBuffers {
		for {
			bufs := [][]byte{prepend, outBuffers[i]}
			head, err = sq.descriptorTable.createDescriptorChain(bufs, 0)
			if err == nil {
				break
			}

			// I don't wanna use errors.Is, it's slow
			//goland:noinspection GoDirectComparisonOfErrors
			if err == ErrNotEnoughFreeDescriptors {
				if waitFree {
					// Wait for more free descriptors to be put back into the queue.
					// If the number of free descriptors is still not sufficient, we'll
					// land here again.
					sq.blockForMoreDescriptors()
					continue
				} else {
					return nil, err
				}
			}
			return nil, fmt.Errorf("create descriptor chain: %w", err)
		}
		chains[i] = head
	}

	// Make the descriptor chain available to the device.
	sq.availableRing.offer(chains)

	// Notify the device to make it process the updated available ring.
	if err := sq.kickEventFD.Kick(); err != nil {
		return chains, fmt.Errorf("notify device: %w", err)
	}

	return chains, nil
}

// GetDescriptorChain returns the device-readable buffers (out buffers) and
// device-writable buffers (in buffers) of the descriptor chain with the given
// head index.
// The head index must be one that was returned by a previous call to
// [SplitQueue.OfferDescriptorChain] and the descriptor chain must not have been
// freed yet.
//
// Be careful to only access the returned buffer slices when the device is no
// longer using them. They must not be accessed after
// [SplitQueue.FreeDescriptorChain] has been called.
func (sq *SplitQueue) GetDescriptorChain(head uint16) (outBuffers, inBuffers [][]byte, err error) {
	sq.ensureInitialized()
	return sq.descriptorTable.getDescriptorChain(head)
}

// FreeDescriptorChain frees the descriptor chain with the given head index.
// The head index must be one that was returned by a previous call to
// [SplitQueue.OfferDescriptorChain] and the descriptor chain must not have been
// freed yet.
//
// This creates new room in the queue which can be used by following
// [SplitQueue.OfferDescriptorChain] calls.
// When there are outstanding calls for [SplitQueue.OfferDescriptorChain] that
// are waiting for free room in the queue, they may become unblocked by this.
func (sq *SplitQueue) FreeDescriptorChain(head uint16) error {
	sq.ensureInitialized()

	//not called under lock
	if err := sq.descriptorTable.freeDescriptorChain(head); err != nil {
		return fmt.Errorf("free: %w", err)
	}

	// There is more free room in the descriptor table now.
	// This is a fire-and-forget signal, so do not block when nobody listens.
	select {
	case sq.moreFreeDescriptors <- struct{}{}:
	default:
	}

	return nil
}

// Close releases all resources used for this queue.
// The implementation will try to release as many resources as possible and
// collect potential errors before returning them.
func (sq *SplitQueue) Close() error {
	var errs []error

	if sq.stop != nil {
		// This has to happen before the event file descriptors may be closed.
		if err := sq.stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop consume used ring: %w", err))
		}

		// The stop function blocked until the goroutine ended, so the channel
		// can now safely be closed.
		close(sq.usedChains)

		// Make sure that this code block is executed only once.
		sq.stop = nil
	}

	if err := sq.kickEventFD.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close kick event file descriptor: %w", err))
	}
	if err := sq.callEventFD.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close call event file descriptor: %w", err))
	}

	if err := sq.descriptorTable.releaseBuffers(); err != nil {
		errs = append(errs, fmt.Errorf("release descriptor buffers: %w", err))
	}

	if sq.buf != nil {
		if err := unix.Munmap(sq.buf); err == nil {
			sq.buf = nil
		} else {
			errs = append(errs, fmt.Errorf("unmap virtqueue buffer: %w", err))
		}
	}

	return errors.Join(errs...)
}

// ensureInitialized is used as a guard to prevent methods to be called on an
// uninitialized instance.
func (sq *SplitQueue) ensureInitialized() {
	if sq.buf == nil {
		panic("used ring is not initialized")
	}
}

func align(index, alignment int) int {
	remainder := index % alignment
	if remainder == 0 {
		return index
	}
	return index + alignment - remainder
}

// splitBuffers processes a list of buffers and splits each buffer that is
// larger than the size limit into multiple smaller buffers.
// If none of the buffers are too big though, do nothing, to avoid allocation for now
func splitBuffers(buffers [][]byte, sizeLimit int) [][]byte {
	for i := range buffers {
		if len(buffers[i]) > sizeLimit {
			return reallySplitBuffers(buffers, sizeLimit)
		}
	}
	return buffers
}

func reallySplitBuffers(buffers [][]byte, sizeLimit int) [][]byte {
	result := make([][]byte, 0, len(buffers))
	for _, buffer := range buffers {
		for added := 0; added < len(buffer); added += sizeLimit {
			if len(buffer)-added <= sizeLimit {
				result = append(result, buffer[added:])
				break
			}
			result = append(result, buffer[added:added+sizeLimit])
		}
	}

	return result
}
