// Package forward implements a dependency-free TCP port forwarder.
//
// It listens on local addresses and relays each accepted connection to a
// destination, which may be a loopback service on the same host or another
// machine the host can reach. It uses only the standard library, so it ships in
// the same single static binary as the scanner. There is no tunnel and no second
// instance: porty runs on one box and forwards from there.
//
// Serve bounds its own resource use so an exposed listener is not trivially
// exhausted: the number of concurrent relays is capped, relays idle in both
// directions are reclaimed after a timeout, a transient Accept error does not
// permanently kill a listener, and a cancelled context tears down in-flight
// relays before Serve returns.
package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// options bounds a Serve run. It is unexported and carries internal defaults so
// the public API stays unchanged; tests construct it directly with smaller
// timeouts. Any non-positive field is filled from defaultOptions in serve.
type options struct {
	dialTimeout   time.Duration // cap on reaching a relay destination
	idleTimeout   time.Duration // reclaim a relay idle in both directions this long
	maxConns      int           // max concurrent relays per Serve call
	acceptBackoff time.Duration // pause after a transient Accept error before retrying
}

func defaultOptions() options {
	return options{
		dialTimeout:   10 * time.Second,
		idleTimeout:   90 * time.Second,
		maxConns:      4096,
		acceptBackoff: 50 * time.Millisecond,
	}
}

// Rule is a single forward: listen on Listen and relay to Dest.
type Rule struct {
	Listen string // bind address, e.g. ":8080" or "127.0.0.1:8080"
	Dest   string // destination host:port, e.g. "10.0.0.5:80"
}

// Listener is a bound forward, ready to serve.
type Listener struct {
	ln   net.Listener
	dest string
}

// Addr returns the address the forward is listening on.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Dest returns the destination connections are relayed to.
func (l *Listener) Dest() string { return l.dest }

// Listen binds a listener for each rule. If any bind fails it closes the
// listeners it already opened and returns the error, so a partial set is never
// left running.
func Listen(rules []Rule) ([]*Listener, error) {
	var out []*Listener
	for _, r := range rules {
		ln, err := net.Listen("tcp", r.Listen)
		if err != nil {
			for _, l := range out {
				l.ln.Close()
			}
			return nil, fmt.Errorf("listen on %q: %w", r.Listen, err)
		}
		out = append(out, &Listener{ln: ln, dest: r.Dest})
	}
	return out, nil
}

// Serve accepts connections on every listener and relays them to their
// destinations until ctx is cancelled. logf, if non-nil, receives non-fatal
// per-connection and per-listener events (an unreachable destination, a relay
// reclaimed for being idle, or a transient accept error).
//
// Serve bounds concurrent relays, reclaims relays that go idle in both
// directions, keeps serving across transient Accept errors, and on cancellation
// closes the listeners and drains in-flight relays before returning.
func Serve(ctx context.Context, listeners []*Listener, logf func(string, ...any)) {
	serve(ctx, listeners, defaultOptions(), logf)
}

func serve(ctx context.Context, listeners []*Listener, opts options, logf func(string, ...any)) {
	def := defaultOptions()
	if opts.dialTimeout <= 0 {
		opts.dialTimeout = def.dialTimeout
	}
	if opts.idleTimeout <= 0 {
		opts.idleTimeout = def.idleTimeout
	}
	if opts.maxConns <= 0 {
		opts.maxConns = def.maxConns
	}
	if opts.acceptBackoff <= 0 {
		opts.acceptBackoff = def.acceptBackoff
	}

	// sem is a counting semaphore that bounds the number of concurrent relays,
	// so a flood of inbound connections cannot exhaust file descriptors or spawn
	// unbounded goroutines. A relay holds one slot for its whole lifetime.
	sem := make(chan struct{}, opts.maxConns)
	var relays sync.WaitGroup

	var accepts sync.WaitGroup
	for _, l := range listeners {
		accepts.Add(1)
		go func(l *Listener) {
			defer accepts.Done()
			acceptLoop(ctx, l, opts, sem, &relays, logf)
		}(l)
	}

	// Closing the listeners unblocks the accept loops on shutdown.
	go func() {
		<-ctx.Done()
		for _, l := range listeners {
			l.ln.Close()
		}
	}()

	accepts.Wait() // listeners closed: no new relays will start
	relays.Wait()  // drain in-flight relays (cancellation closes their conns)
}

func acceptLoop(ctx context.Context, l *Listener, opts options, sem chan struct{}, relays *sync.WaitGroup, logf func(string, ...any)) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // listener closed on shutdown
			}
			// A transient error (for example "too many open files" under load)
			// must not permanently kill the listener: log it, pause briefly, and
			// keep serving once the condition clears.
			if logf != nil {
				logf("accept on %s: %v", l.ln.Addr(), err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(opts.acceptBackoff):
			}
			continue
		}

		// Acquire a relay slot. This bounds concurrent relays; under a flood the
		// accept loop blocks here (further inbound connections queue in the
		// kernel backlog) instead of spawning unbounded goroutines. Cancellation
		// releases us so shutdown never deadlocks.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			conn.Close()
			return
		}

		relays.Add(1)
		go func() {
			defer relays.Done()
			defer func() { <-sem }()
			relay(ctx, conn, l.dest, opts, logf)
		}()
	}
}

func relay(ctx context.Context, src net.Conn, dest string, opts options, logf func(string, ...any)) {
	dialer := net.Dialer{Timeout: opts.dialTimeout}
	dst, err := dialer.DialContext(ctx, "tcp", dest)
	if err != nil {
		if logf != nil {
			logf("dial %s: %v", dest, err)
		}
		src.Close()
		return
	}

	// lastActive holds the unix-nano time of the most recent byte seen in either
	// direction. The watchdog goroutine closes both connections when ctx is
	// cancelled (prompt shutdown) or when the relay has been idle in both
	// directions for idleTimeout (reclaiming slowloris-style holds). Closing the
	// conns unblocks the copy loops below; the watchdog itself stops when the
	// relay finishes, signalled by closing done.
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())
	touch := func() { lastActive.Store(time.Now().UnixNano()) }

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(opts.idleTimeout / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				src.Close()
				dst.Close()
				return
			case <-done:
				return
			case now := <-ticker.C:
				if idle := now.Sub(time.Unix(0, lastActive.Load())); idle >= opts.idleTimeout {
					if logf != nil {
						logf("idle relay %s -> %s closed after %s", src.RemoteAddr(), dest, idle.Round(time.Second))
					}
					src.Close()
					dst.Close()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go copyHalf(&wg, dst, src, touch)
	go copyHalf(&wg, src, dst, touch)
	wg.Wait()

	src.Close()
	dst.Close()
}

// copyHalf copies src into dst, calling onActivity as bytes flow so the relay's
// idle watchdog can tell the connection is alive. A fixed buffer (rather than
// io.Copy) is used so each chunk can mark activity. When src reports EOF it
// half-closes dst's write side, so the peer sees EOF on this direction while the
// other direction keeps flowing.
func copyHalf(wg *sync.WaitGroup, dst, src net.Conn, onActivity func()) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			onActivity()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	// Assert against the CloseWrite interface rather than the concrete
	// *net.TCPConn so the half-close keeps working if dst is ever wrapped.
	if c, ok := dst.(interface{ CloseWrite() error }); ok {
		c.CloseWrite()
	}
}
