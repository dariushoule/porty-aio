package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestForwardRelaysTCP stands up an echo backend, forwards to it, connects
// through the forward, and confirms bytes are relayed in both directions.
func TestForwardRelaysTCP(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				io.Copy(c, c) // echo
				c.Close()
			}(c)
		}
	}()

	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: backend.Addr().String()}})
	if err != nil {
		t.Fatalf("forward listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ls, nil)

	conn, err := net.Dial("tcp", ls[0].Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello porty")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Half-close so the echo backend sees EOF and our ReadAll completes.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Errorf("relayed echo = %q, want %q", got, msg)
	}
}

// TestForwardMultipleRules serves two forwards at once and confirms each relays
// to its own backend.
func TestForwardMultipleRules(t *testing.T) {
	echo := func() net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("backend listen: %v", err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					io.Copy(c, c)
					c.Close()
				}(c)
			}
		}()
		return ln
	}
	b1 := echo()
	defer b1.Close()
	b2 := echo()
	defer b2.Close()

	ls, err := Listen([]Rule{
		{Listen: "127.0.0.1:0", Dest: b1.Addr().String()},
		{Listen: "127.0.0.1:0", Dest: b2.Addr().String()},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ls, nil)

	for i, l := range ls {
		msg := fmt.Sprintf("ping-%d", i)
		if got := roundtrip(t, l.Addr().String(), msg); got != msg {
			t.Errorf("forward %d relayed %q, want %q", i, got, msg)
		}
	}
}

// TestForwardDestUnreachable confirms that when the destination cannot be
// dialed, the relay logs the error and closes the inbound connection.
func TestForwardDestUnreachable(t *testing.T) {
	// A destination nothing listens on: bind an ephemeral port then release it.
	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := tmp.Addr().String()
	tmp.Close()

	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: dead}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var logged string
	logf := func(format string, a ...any) {
		mu.Lock()
		logged = fmt.Sprintf(format, a...)
		mu.Unlock()
	}
	go Serve(ctx, ls, logf)

	conn, err := net.Dial("tcp", ls[0].Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	// The relay fails to reach dead and closes our side, so the read returns EOF.
	if b, _ := io.ReadAll(conn); len(b) != 0 {
		t.Errorf("expected no data from a failed forward, got %q", b)
	}
	conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		l := logged
		mu.Unlock()
		if l != "" {
			if !strings.Contains(l, "dial") {
				t.Errorf("expected a dial error to be logged, got %q", l)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("relay did not log a dial error")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func roundtrip(t *testing.T, addr, msg string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	return string(b)
}

// TestForwardReportsDest checks the accessor used for startup logging.
func TestForwardReportsDest(t *testing.T) {
	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: "10.0.0.5:80"}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ls[0].ln.Close()
	if ls[0].Dest() != "10.0.0.5:80" {
		t.Errorf("Dest() = %q, want 10.0.0.5:80", ls[0].Dest())
	}
}

// TestListenErrorIsClean confirms a bad bind address returns an error rather
// than leaving a half-open set of listeners.
func TestListenErrorIsClean(t *testing.T) {
	if _, err := Listen([]Rule{{Listen: "127.0.0.1:99999", Dest: "x:1"}}); err == nil {
		t.Error("expected an error for an out-of-range bind port")
	}
}

// holdBackend stands up a loopback backend that accepts connections and holds
// them open without ever sending, draining whatever the client writes. It is
// used to exercise the relay lifecycle (idle reclaim, capacity, drain) without a
// peer that closes first. The returned listener is closed via t.Cleanup.
func holdBackend(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				io.Copy(io.Discard, c)
				c.Close()
			}(c)
		}
	}()
	return ln
}

// TestForwardIdleTimeoutReclaims confirms a relay whose peers go silent in both
// directions is reclaimed after the idle timeout instead of leaking forever
// (the slowloris-style hold-open guard).
func TestForwardIdleTimeoutReclaims(t *testing.T) {
	backend := holdBackend(t)

	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: backend.Addr().String()}})
	if err != nil {
		t.Fatalf("forward listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const idle = 300 * time.Millisecond
	go serve(ctx, ls, options{idleTimeout: idle}, nil)

	conn, err := net.Dial("tcp", ls[0].Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	// Send nothing. The relay should be reclaimed for idleness, which the forward
	// signals by closing our connection, so the read returns rather than blocking.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, rerr := conn.Read(make([]byte, 1))
	elapsed := time.Since(start)

	if rerr == nil {
		t.Fatal("expected the idle relay to close our connection, but read returned data")
	}
	if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatalf("relay not reclaimed within 3s (idle timeout %s); our read deadline fired", idle)
	}
	if elapsed < idle/2 {
		t.Errorf("relay closed after %s, sooner than the idle timeout %s should allow", elapsed, idle)
	}
	t.Logf("idle relay reclaimed after %s (idleTimeout=%s)", elapsed.Round(time.Millisecond), idle)
}

// TestForwardConnectionCapBounded confirms the relay semaphore caps how many
// relays run at once, so a burst of inbound connections cannot grow unbounded.
func TestForwardConnectionCapBounded(t *testing.T) {
	const maxConns = 2
	const clients = 5

	var active, peak int32
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				cur := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&peak)
					if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
						break
					}
				}
				io.Copy(io.Discard, c) // hold until the client closes
				atomic.AddInt32(&active, -1)
				c.Close()
			}(c)
		}
	}()

	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: backend.Addr().String()}})
	if err != nil {
		t.Fatalf("forward listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Long idle timeout so no relay is reclaimed mid-test; small connection cap.
	go serve(ctx, ls, options{maxConns: maxConns, idleTimeout: 30 * time.Second}, nil)

	var conns []net.Conn
	for i := 0; i < clients; i++ {
		c, err := net.Dial("tcp", ls[0].Addr().String())
		if err != nil {
			t.Fatalf("dial forward %d: %v", i, err)
		}
		c.Write([]byte{'x'}) // establish the relay so the backend handler runs
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// Wait for the cap to fill, then settle and assert it was never exceeded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&active) < maxConns {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let any over-cap relay surface if the cap failed

	if got := atomic.LoadInt32(&peak); got != maxConns {
		t.Errorf("backend saw a peak of %d concurrent relays, want exactly the cap %d", got, maxConns)
	}
}

// flakyListener wraps a net.Listener and returns a transient (non-ErrClosed)
// error on the first failsLeft Accept calls, to prove the accept loop recovers
// from a transient error (such as EMFILE) instead of dying permanently.
type flakyListener struct {
	net.Listener
	failsLeft atomic.Int32
}

func (f *flakyListener) Accept() (net.Conn, error) {
	if f.failsLeft.Add(-1) >= 0 {
		return nil, transientErr{}
	}
	return f.Listener.Accept()
}

type transientErr struct{}

func (transientErr) Error() string   { return "transient accept failure" }
func (transientErr) Timeout() bool   { return true }
func (transientErr) Temporary() bool { return true }

// TestForwardAcceptLoopSurvivesTransientError confirms a transient Accept error
// is logged and retried rather than permanently killing the listener.
func TestForwardAcceptLoopSurvivesTransientError(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c) // echo
		}
	}()

	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	flaky := &flakyListener{Listener: real}
	flaky.failsLeft.Store(1) // fail the first Accept once, then recover
	l := &Listener{ln: flaky, dest: backend.Addr().String()}

	var mu sync.Mutex
	var gotLog string
	logf := func(format string, a ...any) {
		mu.Lock()
		gotLog += fmt.Sprintf(format, a...) + "\n"
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serve(ctx, []*Listener{l}, options{acceptBackoff: 10 * time.Millisecond}, logf)

	// The first Accept failed transiently; the loop should back off and recover,
	// so a real connection still gets relayed end to end.
	if got := roundtrip(t, real.Addr().String(), "after-transient"); got != "after-transient" {
		t.Errorf("relay after transient accept error = %q, want %q", got, "after-transient")
	}

	mu.Lock()
	logged := gotLog
	mu.Unlock()
	if !strings.Contains(logged, "accept") {
		t.Errorf("expected the transient accept error to be logged, got %q", logged)
	}
}

// TestForwardShutdownDrainsActiveRelay confirms cancellation tears down an
// in-flight relay and Serve returns promptly rather than hanging on it.
func TestForwardShutdownDrainsActiveRelay(t *testing.T) {
	backend := holdBackend(t)

	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: backend.Addr().String()}})
	if err != nil {
		t.Fatalf("forward listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		serve(ctx, ls, options{idleTimeout: 30 * time.Second}, nil)
		close(served)
	}()

	conn, err := net.Dial("tcp", ls[0].Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("active")) // establish an active relay

	time.Sleep(100 * time.Millisecond) // let the relay establish
	cancel()                           // shut down

	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within 3s of cancellation; relays were not drained")
	}

	// The active relay's connection should have been closed by the shutdown.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("expected our connection to be closed on shutdown")
	}
}

// TestDefaultIdleReapIsOff pins the default: idle reaping must be off unless a
// caller opts in with WithIdleTimeout, so the forwarder does not tear down
// long-lived but quiet connections (the ssh -L/socat behavior).
func TestDefaultIdleReapIsOff(t *testing.T) {
	if d := defaultOptions().idleTimeout; d > 0 {
		t.Fatalf("default idleTimeout = %s, want <=0 (idle reaping off by default)", d)
	}
	opts := defaultOptions()
	WithIdleTimeout(45 * time.Second)(&opts)
	if opts.idleTimeout != 45*time.Second {
		t.Fatalf("WithIdleTimeout did not apply: got %s", opts.idleTimeout)
	}
}

// TestForwardIdleReapDisabledKeepsHealthyConn confirms the regression we found:
// with idle reaping off, a connection that is alive but idle at the application
// level (an SSH session at a prompt) is not reaped. A healthy echo backend
// stands in for the server; the client goes idle, then its next request must
// still be relayed.
func TestForwardIdleReapDisabledKeepsHealthyConn(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c) // echo
		}
	}()

	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: backend.Addr().String()}})
	if err != nil {
		t.Fatalf("forward listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serve(ctx, ls, options{}, nil) // idleTimeout zero: reaping off

	conn, err := net.Dial("tcp", ls[0].Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	exchange := func(msg string) (string, error) {
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write([]byte(msg)); err != nil {
			return "", err
		}
		buf := make([]byte, 64)
		conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := conn.Read(buf)
		return string(buf[:n]), err
	}

	if got, err := exchange("hello\n"); err != nil || got != "hello\n" {
		t.Fatalf("initial echo failed: got %q err=%v", got, err)
	}
	time.Sleep(600 * time.Millisecond) // idle, like a user at the prompt
	if got, err := exchange("still there?\n"); err != nil {
		t.Fatalf("idle relay was reaped with reaping off: %v", err)
	} else if got != "still there?\n" {
		t.Fatalf("post-idle echo = %q, want %q", got, "still there?\n")
	}
}

// TestForwardShutdownDrainsWithIdleOff confirms graceful drain still works when
// idle reaping is off: the per-relay watchdog must run for ctx teardown even
// without an idle ticker, or cancellation would hang on a quiet relay.
func TestForwardShutdownDrainsWithIdleOff(t *testing.T) {
	backend := holdBackend(t)

	ls, err := Listen([]Rule{{Listen: "127.0.0.1:0", Dest: backend.Addr().String()}})
	if err != nil {
		t.Fatalf("forward listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		serve(ctx, ls, options{}, nil) // idle off
		close(served)
	}()

	conn, err := net.Dial("tcp", ls[0].Addr().String())
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("active"))

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within 3s of cancellation with idle reaping off")
	}
}
