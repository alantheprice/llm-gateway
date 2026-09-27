package link

import (
	"errors"
	"io"
	"sync"
)

// errPipeOverflow: the consumer abandoned the response; further body
// chunks are discarded until the stream ends.
var errPipeOverflow = errors.New("link: response reader abandoned")

// boundedPipe: an io.Reader backed by a fixed-size buffer. Writes never
// block: when the buffer is full (consumer gone or too slow), the pipe
// latches an overflow error, and subsequent writes are dropped. This
// guarantees the agent's socket read-loop can never block on a response
// nobody is reading.
type boundedPipe struct {
	mu      sync.Mutex
	buf     []byte
	cap     int
	closed  bool
	err     error
	done    chan struct{} // closed when the buffer is drained by a reader OR overflow latches
	drained bool
}

func newBoundedPipe(capacity int) *boundedPipe {
	return &boundedPipe{cap: capacity, done: make(chan struct{})}
}

func (p *boundedPipe) Write(chunk []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errPipeOverflow
	}
	if p.err != nil {
		return 0, p.err
	}
	if len(p.buf)+len(chunk) > p.cap {
		// Overflow: latch the error and wake the writer side.
		p.err = errPipeOverflow
		select {
		case <-p.done:
		default:
			close(p.done)
		}
		return 0, p.err
	}
	p.buf = append(p.buf, chunk...)
	return len(chunk), nil
}

func (p *boundedPipe) CloseWrite() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// Read drains the buffer; blocks while the stream is open and empty.
func (p *boundedPipe) Read(out []byte) (int, error) {
	for {
		p.mu.Lock()
		if len(p.buf) > 0 {
			n := copy(out, p.buf)
			p.buf = p.buf[n:]
			p.mu.Unlock()
			return n, nil
		}
		closed := p.closed
		perr := p.err
		p.mu.Unlock()
		if closed {
			if perr != nil && perr != errPipeOverflow {
				return 0, perr
			}
			return 0, io.EOF
		}
		if perr == errPipeOverflow {
			return 0, perr
		}
		// open and empty: wait for a writer wakeup
		<-p.done
		p.mu.Lock()
		p.err = nil // cleared on drain: reader caught up
		p.mu.Unlock()
	}
}
