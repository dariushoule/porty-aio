// Package scan implements dependency-free TCP connect scanning.
//
// It uses only the standard library net package: a TCP connect scan needs no
// raw sockets, no privileges, and no packet-capture library, which is what lets
// porty-aio ship as a single static binary that runs anywhere (including
// vanilla Windows with no Npcap).
package scan

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result is a single open host:port discovered by a scan.
type Result struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Options configures a scan run.
type Options struct {
	// Concurrency is the maximum number of in-flight connections.
	Concurrency int
	// Timeout is the per-connection dial timeout.
	Timeout time.Duration
}

// Scan dials every host/port combination over TCP and invokes onOpen for each
// port that accepts a connection. onOpen is called serially, so it does not
// need to be safe for concurrent use. Scan blocks until every probe completes
// or ctx is cancelled.
func Scan(ctx context.Context, hosts []string, ports []int, opts Options, onOpen func(Result)) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 512
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 1500 * time.Millisecond
	}

	type job struct {
		host string
		port int
	}

	jobs := make(chan job)
	dialer := net.Dialer{Timeout: opts.Timeout}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				addr := net.JoinHostPort(j.host, strconv.Itoa(j.port))
				conn, err := dialer.DialContext(ctx, "tcp", addr)
				if err != nil {
					continue
				}
				conn.Close()
				mu.Lock()
				onOpen(Result{Host: j.host, Port: j.port})
				mu.Unlock()
			}
		}()
	}

feed:
	for _, h := range hosts {
		for _, p := range ports {
			select {
			case <-ctx.Done():
				break feed
			case jobs <- job{host: h, port: p}:
			}
		}
	}
	close(jobs)
	wg.Wait()
}

// ParseTargets expands a comma-separated list of hosts, IPv4/IPv6 addresses,
// and CIDR ranges into individual target strings.
func ParseTargets(spec string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			ips, err := expandCIDR(part)
			if err != nil {
				return nil, err
			}
			out = append(out, ips...)
			continue
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets parsed from %q", spec)
	}
	return out, nil
}

// maxCIDRHostBits caps CIDR expansion at ~1M addresses (e.g. IPv4 /12). v1
// materializes every address up front, so this guards against accidentally
// expanding an IPv6 /64 or a huge IPv4 range and exhausting memory.
const maxCIDRHostBits = 20

func expandCIDR(cidr string) ([]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ones, bits := ipnet.Mask.Size()
	if hostBits := bits - ones; hostBits > maxCIDRHostBits {
		return nil, fmt.Errorf("CIDR %s is too large to expand (2^%d addresses); narrow the range", cidr, hostBits)
	}

	ip := make(net.IP, len(ipnet.IP))
	copy(ip, ipnet.IP)

	var ips []string
	for ; ipnet.Contains(ip); incIP(ip) {
		ips = append(ips, ip.String())
	}
	// Drop network and broadcast addresses for anything wider than a /31.
	if bits-ones >= 2 && len(ips) > 2 {
		ips = ips[1 : len(ips)-1]
	}
	return ips, nil
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// ParsePorts expands a port specification into a deduplicated, ordered list.
//
// Accepted forms:
//
//	"top"          the built-in common-ports list (default)
//	"all" or "-"   every port, 1-65535
//	"22,80,443"    an explicit list
//	"1-1024"       an inclusive range
//
// Forms may be combined: "22,80,8000-8100".
func ParsePorts(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "top") {
		return append([]int(nil), TopPorts...), nil
	}
	if spec == "-" || strings.EqualFold(spec, "all") {
		return rangePorts(1, 65535), nil
	}

	seen := make(map[int]bool)
	var out []int
	add := func(p int) error {
		if p < 1 || p > 65535 {
			return fmt.Errorf("port out of range: %d", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		return nil
	}

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return nil, fmt.Errorf("invalid port range %q: %w", part, err)
			}
			b, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("invalid port range %q: %w", part, err)
			}
			if a > b {
				a, b = b, a
			}
			for p := a; p <= b; p++ {
				if err := add(p); err != nil {
					return nil, err
				}
			}
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q: %w", part, err)
		}
		if err := add(p); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ports parsed from %q", spec)
	}
	return out, nil
}

func rangePorts(lo, hi int) []int {
	out := make([]int, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		out = append(out, p)
	}
	return out
}

// TopPorts is a compact list of commonly open TCP ports, used by default.
var TopPorts = []int{
	21, 22, 23, 25, 53, 80, 81, 88, 110, 111, 135, 139, 143, 389, 443, 445,
	465, 587, 636, 993, 995, 1025, 1433, 1521, 1723, 2049, 2222, 2375, 2376,
	3000, 3128, 3306, 3389, 4444, 5000, 5432, 5601, 5672, 5900, 5985, 5986,
	6379, 7001, 8000, 8008, 8080, 8081, 8088, 8443, 8500, 8888, 9000, 9090,
	9200, 9300, 9443, 10000, 11211, 15672, 27017, 27018, 50000,
}
