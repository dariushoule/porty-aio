package scan

import (
	"context"
	"net"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestParsePorts(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want []int
	}{
		{"explicit single", "80", []int{80}},
		{"explicit list", "22,80,443", []int{22, 80, 443}},
		{"preserves input order", "443,22,80", []int{443, 22, 80}},
		{"range", "1-3", []int{1, 2, 3}},
		{"reversed range normalized", "3-1", []int{1, 2, 3}},
		{"dedup", "22,22,80", []int{22, 80}},
		{"list plus overlapping range", "80,79-81", []int{80, 79, 81}},
		{"whitespace tolerated", " 22 , 80 ", []int{22, 80}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePorts(tc.spec)
			if err != nil {
				t.Fatalf("ParsePorts(%q) error: %v", tc.spec, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParsePorts(%q) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestParsePortsAllEqualsDash(t *testing.T) {
	all, err := ParsePorts("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 65535 || all[0] != 1 || all[len(all)-1] != 65535 {
		t.Fatalf("all: len=%d first=%d last=%d", len(all), all[0], all[len(all)-1])
	}
	dash, err := ParsePorts("-")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dash, all) {
		t.Error(`ParsePorts("-") should equal ParsePorts("all")`)
	}
}

func TestParsePortsErrors(t *testing.T) {
	for _, spec := range []string{"0", "70000", "abc", "1-abc", "80-", "1-70000"} {
		if _, err := ParsePorts(spec); err == nil {
			t.Errorf("ParsePorts(%q) expected error, got nil", spec)
		}
	}
}

// TestTopPortsGolden pins the default port set: scanning with no -p (or "top")
// must probe exactly TopPorts, and the returned slice must be an independent
// copy so callers cannot corrupt the shared list.
func TestTopPortsGolden(t *testing.T) {
	got, err := ParsePorts("top")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, TopPorts) {
		t.Error(`ParsePorts("top") does not match TopPorts`)
	}
	empty, err := ParsePorts("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty, TopPorts) {
		t.Error(`ParsePorts("") should default to TopPorts`)
	}

	// Mutating the returned slice must not leak into TopPorts.
	first := TopPorts[0]
	got[0] = -1
	if TopPorts[0] != first {
		t.Error("ParsePorts returned a slice aliasing TopPorts; mutation leaked")
	}

	// Sanity: well-known ports are present in the golden set.
	for _, p := range []int{22, 80, 443, 445, 3389} {
		if !contains(TopPorts, p) {
			t.Errorf("TopPorts missing well-known port %d", p)
		}
	}
}

func TestParseTargets(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want []string
	}{
		{"single ip", "10.0.0.1", []string{"10.0.0.1"}},
		{"hostname", "host.lan", []string{"host.lan"}},
		{"comma list", "10.0.0.1,10.0.0.2", []string{"10.0.0.1", "10.0.0.2"}},
		{"cidr /30 drops net+bcast", "192.168.0.0/30", []string{"192.168.0.1", "192.168.0.2"}},
		{"cidr /32", "192.168.0.5/32", []string{"192.168.0.5"}},
		{"cidr /31 keeps both", "192.168.0.0/31", []string{"192.168.0.0", "192.168.0.1"}},
		{"mixed host and cidr", "1.1.1.1,192.168.0.0/30", []string{"1.1.1.1", "192.168.0.1", "192.168.0.2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTargets(tc.spec)
			if err != nil {
				t.Fatalf("ParseTargets(%q) error: %v", tc.spec, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseTargets(%q) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestParseTargetsErrors(t *testing.T) {
	for _, spec := range []string{"", " , ", "10.0.0.0/8", "bad/33"} {
		if _, err := ParseTargets(spec); err == nil {
			t.Errorf("ParseTargets(%q) expected error, got nil", spec)
		}
	}
}

func TestExpandCIDR(t *testing.T) {
	tests := []struct {
		cidr  string
		count int
	}{
		{"192.168.1.0/24", 254},
		{"192.168.1.0/30", 2},
		{"192.168.1.0/31", 2},
		{"192.168.1.7/32", 1},
		{"10.0.0.0/20", 4094},
		{"fe80::/126", 2},
	}
	for _, tc := range tests {
		got, err := expandCIDR(tc.cidr)
		if err != nil {
			t.Fatalf("expandCIDR(%q) error: %v", tc.cidr, err)
		}
		if len(got) != tc.count {
			t.Errorf("expandCIDR(%q) returned %d addresses, want %d", tc.cidr, len(got), tc.count)
		}
	}
}

func TestExpandCIDRGuard(t *testing.T) {
	for _, cidr := range []string{"10.0.0.0/8", "0.0.0.0/0", "172.16.0.0/11", "2001:db8::/64"} {
		if _, err := expandCIDR(cidr); err == nil {
			t.Errorf("expandCIDR(%q) expected too-large error, got nil", cidr)
		}
	}
}

// TestScanDetectsGoldenPortSet is the integration test: it stands up real TCP
// listeners on loopback, scans a port set mixing those open ports with a known
// closed port, and asserts the scanner reports exactly the open (golden) set.
func TestScanDetectsGoldenPortSet(t *testing.T) {
	const numOpen = 6

	var openPorts []int
	for i := 0; i < numOpen; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		go acceptAndClose(ln)
		openPorts = append(openPorts, ln.Addr().(*net.TCPAddr).Port)
	}

	// A known-closed port: bind an ephemeral port, then release it.
	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedPort := tmp.Addr().(*net.TCPAddr).Port
	tmp.Close()

	ports := append([]int{closedPort}, openPorts...)

	var mu sync.Mutex
	var found []int
	Scan(context.Background(), []string{"127.0.0.1"}, ports, Options{
		Concurrency: 16,
		Timeout:     2 * time.Second,
	}, func(r Result) {
		mu.Lock()
		found = append(found, r.Port)
		mu.Unlock()
	})

	sort.Ints(found)
	want := append([]int(nil), openPorts...)
	sort.Ints(want)

	if !reflect.DeepEqual(found, want) {
		t.Errorf("scan found %v, want exactly the open set %v (closed port %d must be absent)",
			found, want, closedPort)
	}
}

func acceptAndClose(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
