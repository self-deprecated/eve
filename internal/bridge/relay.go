package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	// controlPort is where the phone's RemotePairing listener always is.
	controlPort = 49152
	// firstPort and lastPort bound the phone's tunnel ports (the ephemeral range).
	firstPort, lastPort = 49152, 65535
	// windowSize is how many tunnel ports the relay keeps ready ahead of the last one used.
	windowSize = 32
	// refusedHold is how long a client is held when the phone refuses: dropping it straight away
	// makes remotepairingd reconnect in a tight loop.
	refusedHold = 10 * time.Second
)

// relay mirrors TCP ports on the build Mac's LAN address to the same ports on the phone's
// tailnet address. It accepts connections only from that LAN address, which is where
// remotepairingd connects from; anyone else on the network is turned away.
type relay struct {
	local net.IP // the build Mac's LAN address
	phone string // the phone's tailnet address
	log   *slog.Logger
	ctx   context.Context

	mu        sync.Mutex
	listeners map[int]net.Listener
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

func newRelay(ctx context.Context, local net.IP, phone string, log *slog.Logger) *relay {
	return &relay{local: local, phone: phone, log: log, ctx: ctx, listeners: map[int]net.Listener{}, conns: map[net.Conn]struct{}{}}
}

// listen binds the control port. The phone never uses it for a tunnel, since its own listener
// holds it.
func (r *relay) listen() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bind(controlPort)
}

// bind starts mirroring port; r.mu is held.
func (r *relay) bind(port int) error {
	if _, ok := r.listeners[port]; ok {
		return nil
	}
	l, err := net.Listen("tcp", net.JoinHostPort(r.local.String(), strconv.Itoa(port)))
	if err != nil {
		return err
	}
	r.listeners[port] = l
	r.wg.Add(1)
	go r.serve(l, port)
	return nil
}

// slide keeps the window of tunnel ports from port onward bound, and closes the ones behind it.
// Binding the whole range instead would leave the Mac no ports for its own connections from
// this address, remotepairingd's included.
func (r *relay) slide(port int) {
	want := map[int]bool{controlPort: true}
	for i := range windowSize {
		want[wrap(port+i)] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	for p, l := range r.listeners {
		if !want[p] {
			_ = l.Close()
			delete(r.listeners, p)
		}
	}
	for p := range want {
		if err := r.bind(p); err != nil {
			// Usually one of the Mac's own connections holds the port; the next tunnel moves on.
			r.log.Debug("bridge: port unavailable", "port", p, "err", err)
		}
	}
}

// wrap keeps a port inside the tunnel range, past the control port.
func wrap(p int) int {
	if p > lastPort || p <= controlPort {
		return controlPort + 1 + (p-controlPort-1+(lastPort-controlPort))%(lastPort-controlPort)
	}
	return p
}

func (r *relay) serve(l net.Listener, port int) {
	defer r.wg.Done()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handle(c, port)
		}()
	}
}

func (r *relay) handle(c net.Conn, port int) {
	defer func() { _ = c.Close() }()
	if host, _, _ := net.SplitHostPort(c.RemoteAddr().String()); !net.ParseIP(host).Equal(r.local) {
		r.log.Warn("bridge: refused a connection from another machine", "from", c.RemoteAddr().String(), "port", port)
		return
	}
	if !r.track(c, true) {
		return
	}
	defer r.track(c, false)
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	up, err := d.DialContext(r.ctx, "tcp", net.JoinHostPort(r.phone, strconv.Itoa(port)))
	if err != nil {
		r.log.Info("bridge: the phone refused", "port", port, "err", err)
		select {
		case <-time.After(refusedHold):
		case <-r.ctx.Done():
		}
		return
	}
	if !r.track(up, true) {
		_ = up.Close()
		return
	}
	defer r.track(up, false)
	defer func() { _ = up.Close() }()
	// No deadlines: tunnels sit idle between device calls and must stay up.
	done := make(chan struct{})
	go func() {
		pipe(c, up)
		close(done)
	}()
	pipe(up, c)
	<-done
}

// pipe copies src to dst, then closes dst for writing so the other side sees the end.
func pipe(dst, src net.Conn) {
	_, err := io.Copy(dst, src)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		_ = dst.Close()
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// track records an open connection so close can end it; it reports false once the relay is closing.
func (r *relay) track(c net.Conn, add bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !add {
		delete(r.conns, c)
		return true
	}
	if r.ctx.Err() != nil {
		return false
	}
	r.conns[c] = struct{}{}
	return true
}

// close stops every listener and connection; the relay's context must already be cancelled.
func (r *relay) close() {
	r.mu.Lock()
	for p, l := range r.listeners {
		_ = l.Close()
		delete(r.listeners, p)
	}
	for c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// ports returns the bound ports (tests).
func (r *relay) ports() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.listeners))
	for p := range r.listeners {
		out = append(out, p)
	}
	return out
}
