package forward

import (
	"context"
	"io"
	"net"
	"testing"
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
