// Command porty-aio is a compact, dependency-free, cross-platform network tool.
//
// Its core is a TCP connect scanner (no raw sockets, no libpcap/Npcap, no root),
// plus a simple single-box TCP port forwarder. Everything is standard library
// only, so it ships as one static binary that just works anywhere.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dariushoule/porty-aio/internal/forward"
	"github.com/dariushoule/porty-aio/internal/scan"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Subcommand dispatch: "forward" runs the port forwarder; anything else
	// (including a bare target) runs a scan, preserving the original CLI.
	if len(os.Args) > 1 && os.Args[1] == "forward" {
		runForward(os.Args[2:])
		return
	}
	runScan()
}

func runScan() {
	portsSpec := flag.String("p", "top", "ports: 'top', 'all' (or '-'), '22,80,443', or '1-1024'")
	concurrency := flag.Int("c", 512, "maximum concurrent connections")
	timeout := flag.Duration("t", 1500*time.Millisecond, "per-connection timeout")
	jsonOut := flag.Bool("json", false, "emit results as JSON lines")
	showVersion := flag.Bool("version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "porty-aio %s (dependency-free TCP connect scanner)\n\n", version)
		fmt.Fprintf(os.Stderr, "usage: porty-aio [flags] <target>\n")
		fmt.Fprintf(os.Stderr, "       porty-aio forward --listen :8080 --to 10.0.0.5:80\n\n")
		fmt.Fprintf(os.Stderr, "  <target>  host, IP, or CIDR (comma-separated): 10.0.0.0/24,host.lan\n\n")
		fmt.Fprintln(os.Stderr, "flags:")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nexamples:\n")
		fmt.Fprintf(os.Stderr, "  porty-aio 10.0.0.0/24\n")
		fmt.Fprintf(os.Stderr, "  porty-aio -p 1-65535 -c 1024 192.168.1.10\n")
		fmt.Fprintf(os.Stderr, "  porty-aio -p 22,80,443 -json 10.0.0.0/24 > open.jsonl\n")
		fmt.Fprintf(os.Stderr, "\nrun 'porty-aio forward -h' for port forwarding.\n")
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("porty-aio", version)
		return
	}

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	targets, err := scan.ParseTargets(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	ports, err := scan.ParsePorts(*portsSpec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Stream hosts lazily: hostnames resolve once, CIDR ranges expand address by
	// address, so very large ranges are never materialized. Unresolvable
	// hostnames are reported but do not abort the scan.
	onUnresolved := func(name string) {
		fmt.Fprintf(os.Stderr, "warning: could not resolve %q, skipping\n", name)
	}
	var hostCount int
	hosts := func(yield func(string) bool) {
		for h := range scan.StreamHosts(ctx, targets, onUnresolved) {
			hostCount++
			if !yield(h) {
				return
			}
		}
	}

	enc := json.NewEncoder(os.Stdout)
	start := time.Now()
	var open int

	scan.Scan(ctx, hosts, ports, scan.Options{
		Concurrency: *concurrency,
		Timeout:     *timeout,
	}, func(r scan.Result) {
		open++
		if *jsonOut {
			enc.Encode(r)
		} else {
			fmt.Printf("%s:%d\topen\n", r.Host, r.Port)
		}
	})

	fmt.Fprintf(os.Stderr, "done: %d open across %d host(s) x %d port(s) in %s\n",
		open, hostCount, len(ports), time.Since(start).Round(time.Millisecond))
	if hostCount == 0 {
		os.Exit(1)
	}
}

// multiFlag collects a repeatable string flag into a slice, preserving order.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func runForward(args []string) {
	fs := flag.NewFlagSet("forward", flag.ExitOnError)
	var listen, to multiFlag
	fs.Var(&listen, "listen", "address to listen on (repeatable): ':8080' or '127.0.0.1:8080'")
	fs.Var(&to, "to", "destination to relay to (repeatable): '10.0.0.5:80' or '127.0.0.1:3306'")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "porty-aio forward (single-box TCP port forwarder)\n\n")
		fmt.Fprintf(os.Stderr, "usage: porty-aio forward --listen <addr> --to <host:port> [--listen ... --to ...]\n\n")
		fmt.Fprintln(os.Stderr, "flags:")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nexamples:\n")
		fmt.Fprintf(os.Stderr, "  porty-aio forward --listen :8080 --to 10.0.0.5:80\n")
		fmt.Fprintf(os.Stderr, "  porty-aio forward --listen :3306 --to 127.0.0.1:3306\n")
		fmt.Fprintf(os.Stderr, "  porty-aio forward --listen :8080 --to 10.0.0.5:80 --listen :2222 --to 10.0.0.9:22\n")
	}
	fs.Parse(args)

	if len(listen) == 0 || len(listen) != len(to) {
		fmt.Fprintln(os.Stderr, "error: provide an equal number of --listen and --to (at least one pair)")
		fs.Usage()
		os.Exit(2)
	}

	rules := make([]forward.Rule, len(listen))
	for i := range listen {
		rules[i] = forward.Rule{Listen: listen[i], Dest: to[i]}
	}

	listeners, err := forward.Listen(rules)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for _, l := range listeners {
		fmt.Fprintf(os.Stderr, "forwarding %s -> %s\n", l.Addr(), l.Dest())
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}
	forward.Serve(ctx, listeners, logf)
}
