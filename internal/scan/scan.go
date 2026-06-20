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
	"iter"
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
// port that accepts a connection. Hosts are pulled lazily from the hosts
// iterator, so callers can stream very large target sets without materializing
// them. onOpen is called serially, so it does not need to be safe for
// concurrent use. Scan blocks until every probe completes or ctx is cancelled.
func Scan(ctx context.Context, hosts iter.Seq[string], ports []int, opts Options, onOpen func(Result)) {
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
	for h := range hosts {
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

// Target is a parsed scan target: a single IP, a CIDR range, or a hostname.
// CIDR ranges and hostnames are expanded lazily by StreamHosts so large target
// sets are never materialized.
type Target struct {
	raw  string
	ip   net.IP
	cidr *net.IPNet
	host string
}

// maxCIDRHostBits bounds how large a single CIDR range may be. With streaming
// this is no longer a memory limit; it is a footgun guard, primarily against
// accidentally enumerating an IPv6 range (a /64 is 2^64 addresses). 24 host
// bits allows an IPv4 /8 (~16M addresses); raise it if you really need wider.
const maxCIDRHostBits = 24

// ParseTargets parses a comma-separated list of hosts, IPv4/IPv6 addresses, and
// CIDR ranges into Targets. It validates syntax and rejects CIDR ranges larger
// than maxCIDRHostBits, but does not expand ranges or resolve hostnames; that
// happens lazily in StreamHosts.
func ParseTargets(spec string) ([]Target, error) {
	var out []Target
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			_, ipnet, err := net.ParseCIDR(part)
			if err != nil {
				return nil, err
			}
			if ones, bits := ipnet.Mask.Size(); bits-ones > maxCIDRHostBits {
				return nil, fmt.Errorf("CIDR %s is too large (2^%d addresses); narrow the range", part, bits-ones)
			}
			out = append(out, Target{raw: part, cidr: ipnet})
			continue
		}
		if ip := net.ParseIP(part); ip != nil {
			out = append(out, Target{raw: part, ip: ip})
			continue
		}
		out = append(out, Target{raw: part, host: part})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets parsed from %q", spec)
	}
	return out, nil
}

// StreamHosts lazily yields the IP addresses to scan for the given targets: IP
// literals as-is, CIDR ranges expanded address by address, and hostnames
// resolved (once each) to their IPs. Ranges and hostnames are produced lazily,
// so a large CIDR is never materialized. Explicit IPs and resolved hostname
// addresses are deduplicated; CIDR addresses are streamed without a global dedup
// set (a single range never repeats an address), so overlapping ranges may be
// scanned more than once. Hostnames that fail to resolve are passed to
// onUnresolved (which may be nil) and skipped.
func StreamHosts(ctx context.Context, targets []Target, onUnresolved func(string)) iter.Seq[string] {
	return func(yield func(string) bool) {
		var resolver net.Resolver
		seen := make(map[string]bool)
		emit := func(ip string) bool {
			if seen[ip] {
				return true
			}
			seen[ip] = true
			return yield(ip)
		}

		for _, t := range targets {
			switch {
			case t.ip != nil:
				if !emit(t.ip.String()) {
					return
				}
			case t.cidr != nil:
				if !eachCIDRHost(t.cidr, yield) {
					return
				}
			default:
				addrs, err := resolver.LookupIP(ctx, "ip", t.host)
				if err != nil || len(addrs) == 0 {
					if onUnresolved != nil {
						onUnresolved(t.host)
					}
					continue
				}
				for _, a := range addrs {
					if !emit(a.String()) {
						return
					}
				}
			}
		}
	}
}

// eachCIDRHost calls yield for each scannable address in the range. For ranges
// wider than a /31 it skips the network and broadcast addresses. It returns
// false if yield asked to stop.
func eachCIDRHost(ipnet *net.IPNet, yield func(string) bool) bool {
	ones, bits := ipnet.Mask.Size()
	skipEnds := bits-ones >= 2

	network := ipnet.IP.Mask(ipnet.Mask)
	broadcast := lastAddr(ipnet)

	ip := make(net.IP, len(network))
	copy(ip, network)
	for ipnet.Contains(ip) {
		if !(skipEnds && (ip.Equal(network) || ip.Equal(broadcast))) {
			if !yield(ip.String()) {
				return false
			}
		}
		incIP(ip)
	}
	return true
}

// lastAddr returns the highest address in the range (all host bits set).
func lastAddr(ipnet *net.IPNet) net.IP {
	network := ipnet.IP.Mask(ipnet.Mask)
	out := make(net.IP, len(network))
	for i := range network {
		out[i] = network[i] | ^ipnet.Mask[i]
	}
	return out
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
