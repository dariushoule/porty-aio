package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// portyBin is the freshly built CLI under test, set up by TestMain.
var portyBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "porty-cli")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	portyBin = filepath.Join(dir, "porty")
	if runtime.GOOS == "windows" {
		portyBin += ".exe"
	}
	build := exec.Command("go", "build", "-o", portyBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func runPorty(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, portyBin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), errb.String(), ee.ExitCode()
		}
		t.Fatalf("run %v: %v", args, err)
	}
	return out.String(), errb.String(), 0
}

// TestCLIScanOutputModes runs the built binary against a real loopback listener
// and checks both text and JSON output, exercising the default scan dispatch.
func TestCLIScanOutputModes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port)

	out, _, code := runPorty(t, "-p", port, "-t", "2s", "127.0.0.1")
	if code != 0 {
		t.Fatalf("text scan exit=%d", code)
	}
	if !strings.Contains(out, "127.0.0.1:"+port) {
		t.Errorf("text output missing open port; got %q", out)
	}

	out, _, code = runPorty(t, "-p", port, "-t", "2s", "-json", "127.0.0.1")
	if code != 0 {
		t.Fatalf("json scan exit=%d", code)
	}
	if !strings.Contains(out, `"port":`+port) {
		t.Errorf("json output missing port; got %q", out)
	}
}

// TestCLIForwardDispatch confirms "forward" routes to the forwarder and reports
// a usage error (exit 2) when no rules are given.
func TestCLIForwardDispatch(t *testing.T) {
	_, stderr, code := runPorty(t, "forward")
	if code != 2 {
		t.Errorf("forward with no args exit=%d, want 2", code)
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("expected usage on stderr, got %q", stderr)
	}
}

func TestCLIVersion(t *testing.T) {
	out, _, code := runPorty(t, "-version")
	if code != 0 {
		t.Fatalf("version exit=%d", code)
	}
	if !strings.Contains(out, "porty-aio") {
		t.Errorf("version output = %q", out)
	}
}
