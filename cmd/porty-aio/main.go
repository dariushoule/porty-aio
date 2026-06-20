// Command porty-aio is a compact, dependency-free, cross-platform port scanner.
//
// v1 is a TCP connect scanner only: no raw sockets, no libpcap/Npcap, no root.
// That constraint is the whole point: one static binary that just works
// anywhere. Forwarding/pivot features land in later versions.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/dariushoule/porty-aio/internal/scan"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	portsSpec := flag.String("p", "top", "ports: 'top', 'all' (or '-'), '22,80,443', or '1-1024'")
	concurrency := flag.Int("c", 512, "maximum concurrent connections")
	timeout := flag.Duration("t", 1500*time.Millisecond, "per-connection timeout")
	jsonOut := flag.Bool("json", false, "emit results as JSON lines")
	showVersion := flag.Bool("version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "porty-aio %s (dependency-free TCP connect scanner)\n\n", version)
		fmt.Fprintf(os.Stderr, "usage: porty-aio [flags] <target>\n\n")
		fmt.Fprintf(os.Stderr, "  <target>  host, IP, or CIDR (comma-separated): 10.0.0.0/24,host.lan\n\n")
		fmt.Fprintln(os.Stderr, "flags:")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nexamples:\n")
		fmt.Fprintf(os.Stderr, "  porty-aio 10.0.0.0/24\n")
		fmt.Fprintf(os.Stderr, "  porty-aio -p 1-65535 -c 1024 192.168.1.10\n")
		fmt.Fprintf(os.Stderr, "  porty-aio -p 22,80,443 -json 10.0.0.0/24 > open.jsonl\n")
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

	hosts, err := scan.ParseTargets(flag.Arg(0))
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
		open, len(hosts), len(ports), time.Since(start).Round(time.Millisecond))
}
