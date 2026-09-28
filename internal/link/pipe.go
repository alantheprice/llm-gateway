package link

import (
	"errors"
	"io"
	"sync"
)

// errPipeOverflow: the consumer stopped reading (abandoned failover
// loser, stalled client); the response is cut off rather than letting it
// block the socket.
var errPipeOverflow = errors.New("link: response reader abandoned")

// bufPipe carries one relayed response body from the agent socket's read
// loop to the HTTP relay. Unlike io.Pipe, Write never blocks: the read
// loop serves every request on the link, so one slow or abandoned reader
// must not stall the others. Data buffers up to a cap; past it the pipe
// closes with errPipeOverflow and further writes are dropped.
type bufPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	cap    int
	closed bool
	err    error         // reported to the reader once the buffer drains (nil = EOF)
	done   chan struct{} // closed when the pipe closes
}

func newBufPipe(capacity int) *bufPipe {
	p := &bufPipe{cap: capacity, done: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *bufPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		if p.err != nil {
			return 0, p.err
		}
		return 0, io.ErrClosedPipe
	}
	if len(p.buf)+len(b) > p.cap {
		p.closeLocked(errPipeOverflow)
		return 0, errPipeOverflow
	}
	p.buf = append(p.buf, b...)
	p.cond.Broadcast()
	return len(b), nil
}

// Read blocks until data arrives or the pipe closes; buffered data is
// always delivered before the close error.
func (p *bufPipe) Read(out []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.buf) > 0 {
		n := copy(out, p.buf)
		p.buf = p.buf[n:]
		if len(p.buf) == 0 {
			p.buf = nil // release the backing array between bursts
		}
		return n, nil
	}
	if p.err != nil {
		return 0, p.err
	}
	return 0, io.EOF
}

// Close ends the body normally (reader sees EOF after draining).
func (p *bufPipe) Close() error { return p.CloseWithError(nil) }

// CloseWithError ends the body with err (nil = EOF). First close wins.
func (p *bufPipe) CloseWithError(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked(err)
	return nil
}

func (p *bufPipe) closeLocked(err error) {
	if p.closed {
		return
	}
	p.closed = true
	p.err = err
	close(p.done)
	p.cond.Broadcast()
}
