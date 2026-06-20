package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
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
