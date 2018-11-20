package stats

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"sync"
	"time"

	logger "github.com/sirupsen/logrus"
)

// TODO(btc): add constructor that accepts functional options in order to allow
// users to choose the constants that work best for them. (Leave the existing
// c'tor for backwards compatibility)
// e.g. `func NewNetworkStatsdSinkWithOptions(opts ...Option) Sink`

const (
	flushInterval           = time.Second
	logOnEveryNDroppedBytes = 1 << 15 // Log once per 32kb of dropped stats
	defaultBufferSize       = 1 << 16
	approxMaxMemBytes       = 1 << 22
	chanSize                = approxMaxMemBytes / defaultBufferSize
)

// NewNetworkStatsdSink returns a FlushableSink that is backed by a buffered writer
// and a separate goroutine that flushes those buffers to a statsd connection.
func NewNetworkStatsdSink() FlushableSink {
	outc := make(chan *bytes.Buffer, chanSize) // TODO(btc): parameterize
	writer := sinkWriter{
		outc: outc,
	}
	bufWriter := bufio.NewWriterSize(&writer, defaultBufferSize) // TODO(btc): parameterize size
	pool := newBufferPool(defaultBufferSize)
	mu := &sync.Mutex{}
	flushCond := sync.NewCond(mu)
	s := &networkStatsdSink{
		outc:      outc,
		bufWriter: bufWriter,
		pool:      pool,
		mu:        mu,
		flushCond: flushCond,
	}
	writer.pool = s.pool
	go s.run()
	return s
}

type networkStatsdSink struct {
	conn net.Conn
	outc chan *bytes.Buffer
	pool *bufferpool

	mu            *sync.Mutex
	droppedBytes  uint64
	bufWriter     *bufio.Writer
	flushCond     *sync.Cond
	lastFlushTime time.Time
}

type sinkWriter struct {
	pool *bufferpool
	outc chan<- *bytes.Buffer
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	n := len(p)
	dest := w.pool.Get()
	dest.Write(p)
	select {
	case w.outc <- dest:
		return n, nil
	default:
		return 0, fmt.Errorf("statsd channel full, dropping stats buffer with %d bytes", n)
	}
}

func (s *networkStatsdSink) Flush() {
	now := time.Now()
	if err := s.flush(); err != nil {
		// Not much we can do here; we don't know how/why we failed.
		return
	}
	s.mu.Lock()
	for now.After(s.lastFlushTime) {
		s.flushCond.Wait()
	}
	s.mu.Unlock()
}

func (s *networkStatsdSink) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.bufWriter.Flush()
	if err != nil {
		s.handleFlushError(err, s.bufWriter.Buffered())
		return err
	}
	return nil
}

func (s *networkStatsdSink) flushString(f string, args ...interface{}) {
	s.mu.Lock()
	_, err := fmt.Fprintf(s.bufWriter, f, args...)
	if err != nil {
		s.handleFlushError(err, s.bufWriter.Buffered())
	}
	s.mu.Unlock()
}

// s.mu should be held
func (s *networkStatsdSink) handleFlushError(err error, droppedBytes int) {
	d := uint64(droppedBytes)
	if (s.droppedBytes+d)%logOnEveryNDroppedBytes > s.droppedBytes%logOnEveryNDroppedBytes {
		logger.WithField("total_dropped_bytes", s.droppedBytes+d).
			WithField("dropped_bytes", d).
			Error(err)
	}
	s.droppedBytes += d

	s.bufWriter.Reset(&sinkWriter{
		pool: s.pool,
		outc: s.outc,
	})
}

func (s *networkStatsdSink) FlushCounter(name string, value uint64) {
	s.flushString("%s:%d|c\n", name, value)
}

func (s *networkStatsdSink) FlushGauge(name string, value uint64) {
	s.flushString("%s:%d|g\n", name, value)
}

func (s *networkStatsdSink) FlushTimer(name string, value float64) {
	s.flushString("%s:%f|ms\n", name, value)
}

func (s *networkStatsdSink) run() {
	settings := GetSettings()
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		if s.conn == nil {
			conn, err := net.Dial(settings.StatsdProtocol, fmt.Sprintf("%s:%d", settings.StatsdHost,
				settings.StatsdPort))
			if err != nil {
				logger.Warnf("statsd connection error: %s", err)
				time.Sleep(3 * time.Second)
				continue
			}
			s.conn = conn
		}

		select {
		case <-t.C:
			s.flush()
		case buf, ok := <-s.outc: // Receive from the channel and check if the channel has been closed
			if !ok {
				logger.Warnf("Closing statsd client")
				s.conn.Close()
				return
			}
			lenbuf := len(buf.Bytes())
			n, err := s.conn.Write(buf.Bytes())

			if len(s.outc) == 0 {
				// We've at least tried to write all the data we have. Wake up anyone waiting on flush.
				s.mu.Lock()
				s.lastFlushTime = time.Now()
				s.mu.Unlock()
				s.flushCond.Broadcast()
			}

			if err != nil || n < lenbuf {
				s.mu.Lock()
				if err != nil {
					s.handleFlushError(err, lenbuf)
				} else {
					s.handleFlushError(fmt.Errorf("short write to statsd, resetting connection"), lenbuf-n)
				}
				s.mu.Unlock()
				_ = s.conn.Close() // Ignore close failures
				s.conn = nil
			}
			s.pool.Put(buf)
		}
	}
}

type bufferpool struct {
	pool sync.Pool
}

func newBufferPool(defaultSizeBytes int) *bufferpool {
	p := new(bufferpool)
	p.pool.New = func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, defaultSizeBytes))
	}
	return p
}

func (p *bufferpool) Put(b *bytes.Buffer) {
	b.Reset()
	p.pool.Put(b)
}

func (p *bufferpool) Get() *bytes.Buffer {
	return p.pool.Get().(*bytes.Buffer)
}
