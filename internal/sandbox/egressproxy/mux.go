package egressproxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// The multiplex between the in-sandbox forwarder and the kernel's Session.
// One byte stream (a docker sidecar's stdio, an inherited socketpair) carries
// many TCP connections as frames:
//
//	type (1 byte) | stream id (uint32, big endian) | length (uint32) | payload
//
// Types: 'R' the forwarder is listening (id 0, no payload; the kernel starts
// the server only after it); 'O' a connection was accepted (forwarder →
// kernel); 'D' bytes for a stream (both ways); 'C' the stream is closed (both
// ways). The forwarder is the only side that opens streams, so the kernel
// never dials anything the server did not ask for through CONNECT.
//
// The Python forwarder (forwarder.py, the docker runner's sidecar) speaks the
// same frames; TestPythonForwarderSpeaksTheMux checks the two agree.
const (
	frameReady = 'R'
	frameOpen  = 'O'
	frameData  = 'D'
	frameClose = 'C'

	frameHeader = 9
	// maxFrame bounds one payload; a larger length is a protocol error (a
	// corrupted or hostile stream never makes the kernel allocate more).
	maxFrame = 64 << 10
)

// ErrProtocol is a malformed frame on the multiplex.
var ErrProtocol = errors.New("egressproxy: multiplex protocol error")

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (fw *frameWriter) write(typ byte, id uint32, p []byte) error {
	var h [frameHeader]byte
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:5], id)
	binary.BigEndian.PutUint32(h[5:9], uint32(len(p)))
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.w.Write(h[:]); err != nil {
		return err
	}
	if len(p) > 0 {
		if _, err := fw.w.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// data writes p as one or more data frames.
func (fw *frameWriter) data(id uint32, p []byte) error {
	for len(p) > 0 {
		n := len(p)
		if n > maxFrame {
			n = maxFrame
		}
		if err := fw.write(frameData, id, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

func readFrame(r *bufio.Reader, buf []byte) (typ byte, id uint32, p []byte, err error) {
	var h [frameHeader]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return 0, 0, nil, err
	}
	typ, id = h[0], binary.BigEndian.Uint32(h[1:5])
	n := binary.BigEndian.Uint32(h[5:9])
	if n > maxFrame {
		return 0, 0, nil, fmt.Errorf("%w: frame of %d bytes", ErrProtocol, n)
	}
	switch typ {
	case frameReady, frameOpen, frameData, frameClose:
	default:
		return 0, 0, nil, fmt.Errorf("%w: frame type %q", ErrProtocol, typ)
	}
	p = buf[:n]
	if _, err = io.ReadFull(r, p); err != nil {
		return 0, 0, nil, err
	}
	return typ, id, p, nil
}

// serveMux is the kernel side: every stream the forwarder opens becomes one
// net.Conn handed to handle. It returns when the multiplex ends (the
// forwarder exited, or rwc was closed) after every stream goroutine it started
// has returned. ready is called once, on the forwarder's 'R' frame.
func serveMux(rwc io.ReadWriteCloser, ready func(), handle func(net.Conn)) error {
	fw := &frameWriter{w: rwc}
	r := bufio.NewReaderSize(rwc, maxFrame+frameHeader)
	buf := make([]byte, maxFrame)
	var wg sync.WaitGroup
	streams := map[uint32]net.Conn{}
	var mu sync.Mutex
	closeStream := func(id uint32) {
		mu.Lock()
		c, ok := streams[id]
		delete(streams, id)
		mu.Unlock()
		if ok {
			_ = c.Close()
		}
	}
	var err error
	for {
		var typ byte
		var id uint32
		var p []byte
		typ, id, p, err = readFrame(r, buf)
		if err != nil {
			break
		}
		switch typ {
		case frameReady:
			if ready != nil {
				ready()
				ready = nil
			}
		case frameOpen:
			mu.Lock()
			_, dup := streams[id]
			mu.Unlock()
			if dup {
				err = fmt.Errorf("%w: stream %d opened twice", ErrProtocol, id)
				break
			}
			kernelEnd, proxyEnd := net.Pipe()
			mu.Lock()
			streams[id] = kernelEnd
			mu.Unlock()
			wg.Add(2)
			go func() {
				defer wg.Done()
				handle(proxyEnd)
				_ = proxyEnd.Close()
			}()
			go func(id uint32, c net.Conn) {
				defer wg.Done()
				b := make([]byte, 32<<10)
				for {
					n, rerr := c.Read(b)
					if n > 0 {
						if fw.data(id, b[:n]) != nil {
							break
						}
					}
					if rerr != nil {
						break
					}
				}
				mu.Lock()
				_, open := streams[id]
				delete(streams, id)
				mu.Unlock()
				_ = c.Close()
				if open {
					_ = fw.write(frameClose, id, nil)
				}
			}(id, kernelEnd)
		case frameData:
			mu.Lock()
			c, ok := streams[id]
			mu.Unlock()
			if ok {
				// A stream whose handler stopped reading is closed rather
				// than left to block every other stream behind it forever.
				if _, werr := c.Write(p); werr != nil {
					closeStream(id)
				}
			}
		case frameClose:
			closeStream(id)
		}
		if err != nil {
			break
		}
	}
	_ = rwc.Close()
	mu.Lock()
	for id, c := range streams {
		_ = c.Close()
		delete(streams, id)
	}
	mu.Unlock()
	wg.Wait()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Forward is the forwarder side: it announces itself ('R'), then relays every
// connection accepted on ln over rwc as one stream. It returns when rwc ends
// (the kernel closed the session) or ln is closed, after closing ln and every
// relayed connection.
func Forward(ln net.Listener, rwc io.ReadWriteCloser) error {
	fw := &frameWriter{w: rwc}
	if err := fw.write(frameReady, 0, nil); err != nil {
		return err
	}
	var (
		mu    sync.Mutex
		conns = map[uint32]net.Conn{}
		next  uint32
		wg    sync.WaitGroup
	)
	readDone := make(chan error, 1)
	go func() {
		r := bufio.NewReaderSize(rwc, maxFrame+frameHeader)
		buf := make([]byte, maxFrame)
		for {
			typ, id, p, err := readFrame(r, buf)
			if err != nil {
				readDone <- err
				_ = ln.Close()
				return
			}
			mu.Lock()
			c := conns[id]
			if typ == frameClose {
				delete(conns, id)
			}
			mu.Unlock()
			if c == nil {
				continue
			}
			switch typ {
			case frameData:
				if _, err := c.Write(p); err != nil {
					_ = c.Close()
				}
			case frameClose:
				_ = c.Close()
			}
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		mu.Lock()
		next++
		id := next
		conns[id] = c
		mu.Unlock()
		if fw.write(frameOpen, id, nil) != nil {
			_ = c.Close()
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := make([]byte, 32<<10)
			for {
				n, rerr := c.Read(b)
				if n > 0 && fw.data(id, b[:n]) != nil {
					break
				}
				if rerr != nil {
					break
				}
			}
			mu.Lock()
			_, open := conns[id]
			delete(conns, id)
			mu.Unlock()
			_ = c.Close()
			if open {
				_ = fw.write(frameClose, id, nil)
			}
		}()
	}
	_ = ln.Close()
	_ = rwc.Close()
	mu.Lock()
	for id, c := range conns {
		_ = c.Close()
		delete(conns, id)
	}
	mu.Unlock()
	wg.Wait()
	err := <-readDone
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}
