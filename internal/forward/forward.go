// Package forward implements a dependency-free TCP port forwarder.
//
// It listens on local addresses and relays each accepted connection to a
// destination, which may be a loopback service on the same host or another
// machine the host can reach. It uses only the standard library, so it ships in
// the same single static binary as the scanner. There is no tunnel and no second
// instance: porty runs on one box and forwards from there.
package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// dialTimeout bounds how long a relayed connection waits to reach its
// destination before giving up.
const dialTimeout = 10 * time.Second

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
// per-connection errors (such as an unreachable destination).
func Serve(ctx context.Context, listeners []*Listener, logf func(string, ...any)) {
	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Add(1)
		go func(l *Listener) {
			defer wg.Done()
			acceptLoop(l, logf)
		}(l)
	}

	// Closing the listeners unblocks the accept loops on shutdown.
	go func() {
		<-ctx.Done()
		for _, l := range listeners {
			l.ln.Close()
		}
	}()

	wg.Wait()
}

func acceptLoop(l *Listener, logf func(string, ...any)) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // listener closed (shutdown)
		}
		go relay(conn, l.dest, logf)
	}
}

func relay(src net.Conn, dest string, logf func(string, ...any)) {
	dst, err := net.DialTimeout("tcp", dest, dialTimeout)
	if err != nil {
		if logf != nil {
			logf("dial %s: %v", dest, err)
		}
		src.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go copyHalf(&wg, dst, src)
	go copyHalf(&wg, src, dst)
	wg.Wait()

	src.Close()
	dst.Close()
}

// copyHalf copies src into dst, then half-closes dst's write side so the peer
// sees EOF on this direction while the other direction keeps flowing.
func copyHalf(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	io.Copy(dst, src)
	if c, ok := dst.(*net.TCPConn); ok {
		c.CloseWrite()
	}
}
