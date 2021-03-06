package statsd

import (
	"bytes"
	"fmt"
	"sync"
	"time"
)

var senderPool = newBufferPool()

// BufferedSender provides a buffered statsd udp, sending multiple
// metrics, where possible.
type BufferedSender struct {
	flushBytes    int
	flushInterval time.Duration
	sender        Sender
	// lifecycle
	shutdown chan chan error
	running  bool
	runmx    sync.RWMutex
	// buffers
	bufmx  sync.Mutex
	buffer *bytes.Buffer
	bufs   chan *bytes.Buffer
}

// Send bytes.
func (s *BufferedSender) Send(data []byte) (int, error) {
	s.runmx.RLock()
	defer s.runmx.RUnlock()
	if !s.running {
		return 0, fmt.Errorf("BufferedSender is not running")
	}

	s.withBufferLock(func() {
		blen := s.buffer.Len()
		if blen > 0 && blen+len(data)+1 >= s.flushBytes {
			s.swapnqueue()
		}

		s.buffer.Write(data)
		s.buffer.WriteByte('\n')

		if s.buffer.Len() >= s.flushBytes {
			s.swapnqueue()
		}
	})
	return len(data), nil
}

// Close Buffered Sender
func (s *BufferedSender) Close() error {
	// since we are running, write lock during cleanup
	s.runmx.Lock()
	defer s.runmx.Unlock()
	if !s.running {
		return nil
	}

	errChan := make(chan error)
	s.running = false
	s.shutdown <- errChan
	return <-errChan
}

// Start Buffered Sender
// Begins ticker and read loop
func (s *BufferedSender) Start() {
	// write lock to start running
	s.runmx.Lock()
	defer s.runmx.Unlock()
	if s.running {
		return
	}

	s.running = true
	s.bufs = make(chan *bytes.Buffer, 32)
	go s.run()
}

func (s *BufferedSender) withBufferLock(fn func()) {
	s.bufmx.Lock()
	defer s.bufmx.Unlock()
	fn()
}

func (s *BufferedSender) swapnqueue() {
	if s.buffer.Len() == 0 {
		return
	}
	ob := s.buffer
	nb := senderPool.Get()
	s.buffer = nb
	s.bufs <- ob
}

func (s *BufferedSender) run() {
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	doneChan := make(chan bool)
	go func() {
		for buf := range s.bufs {
			s.flush(buf)
			senderPool.Put(buf)
		}
		doneChan <- true
	}()

	for {
		select {
		case <-ticker.C:
			s.withBufferLock(func() {
				s.swapnqueue()
			})
		case errChan := <-s.shutdown:
			s.withBufferLock(func() {
				s.swapnqueue()
			})
			close(s.bufs)
			<-doneChan
			errChan <- s.sender.Close()
			return
		}
	}
}

// send to remove endpoint and truncate buffer
func (s *BufferedSender) flush(b *bytes.Buffer) (int, error) {
	n, err := s.sender.Send(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	b.Truncate(0) // clear the buffer
	return n, err
}

// Returns a new BufferedSender
//
// addr is a string of the format "hostname:port", and must be parsable by
// net.ResolveUDPAddr.
//
// flushInterval is a time.Duration, and specifies the maximum interval for
// packet sending. Note that if you send lots of metrics, you will send more
// often. This is just a maximal threshold.
//
// flushBytes specifies the maximum udp packet size you wish to send. If adding
// a metric would result in a larger packet than flushBytes, the packet will
// first be send, then the new data will be added to the next packet.
func NewBufferedSender(addr string, flushInterval time.Duration, flushBytes int) (Sender, error) {
	simpleSender, err := NewSimpleSender(addr)
	if err != nil {
		return nil, err
	}

	sender := &BufferedSender{
		flushBytes:    flushBytes,
		flushInterval: flushInterval,
		sender:        simpleSender,
		buffer:        bytes.NewBuffer(make([]byte, 0, flushBytes)),
		shutdown:      make(chan chan error),
	}

	sender.Start()
	return sender, nil
}
