package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// runHealth implements `sorotrail health`: GET the API's /health
// endpoint and exit 0 on 2xx, nonzero otherwise. It is the probe an
// operator wires into external checks — a k8s liveness/readiness
// probe, a load-balancer health check, a CI gate, a cron alert —
// where the caller needs a plain exit code rather than parsed JSON.
//
// Compared with `sorotrail healthcheck` (the in-container docker
// HEALTHCHECK probe), `health` adds --url so the same binary can
// probe a remote deployment over http:// or https:// from outside
// the container.
//
// Exit codes mirror healthcheck so probe semantics are identical:
//
//	0  the endpoint returned 2xx — the API is healthy
//	1  non-2xx, network error, or timeout
//	2  flag/usage error
func runHealth(args []string) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: sorotrail health [flags]

Probes the API's /health endpoint and exits 0 on a 2xx response,
1 on any failure (non-2xx, network error, timeout), 2 on a usage
error. Intended for probes: k8s liveness/readiness checks, load
balancer health checks, CI gates, and monitoring scripts that need
a plain exit code instead of curl.

By default it probes the local deployment like `+"`healthcheck`"+` does
(--addr / $HTTP_ADDR / 127.0.0.1:8080, endpoint /health). Pass
--url to probe a full remote endpoint instead.

flags:
`)
		fs.PrintDefaults()
	}
	urlFlag := fs.String("url", "",
		"full URL to probe (e.g. https://api.example.com/health); overrides --addr/--endpoint")
	addrFlag := fs.String("addr", "",
		"host:port to probe (defaults to $HTTP_ADDR or 127.0.0.1:8080)")
	endpoint := fs.String("endpoint", healthcheckEndpointDefault,
		"URL path probed on the indexer (e.g. /health, /livez)")
	timeout := fs.Duration("timeout", healthcheckTimeoutDefault,
		"HTTP client timeout for the probe")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // usage already printed; asking for help is not a failure
		}
		return healthcodeExitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintln(fs.Output(), "health: --timeout must be positive")
		return healthcodeExitUsage
	}

	var target string
	if *urlFlag != "" {
		if flagSet(fs, "addr") || flagSet(fs, "endpoint") {
			fmt.Fprintln(fs.Output(),
				"health: --url cannot be combined with --addr or --endpoint")
			return healthcodeExitUsage
		}
		if err := validateProbeURL(*urlFlag); err != nil {
			fmt.Fprintln(fs.Output(), "health:", err)
			return healthcodeExitUsage
		}
		target = *urlFlag
	} else {
		if *endpoint == "" || !strings.HasPrefix(*endpoint, "/") {
			fmt.Fprintln(fs.Output(),
				"health: --endpoint must be a path beginning with '/'")
			return healthcodeExitUsage
		}
		target = "http://" + resolveHealthcheckAddr(*addrFlag) + *endpoint
	}
	return probeEndpoint("health", target, *timeout)
}

// flagSet reports whether the named flag was explicitly provided.
// Comparing values against defaults can't detect `--url x --endpoint
// /health` (the endpoint equals its default), and mixing a full URL
// with an address/path override must be rejected either way.
func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// validateProbeURL rejects URLs the probe cannot meaningfully hit:
// anything without an http(s) scheme, or without a host.
func validateProbeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("--url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("--url must be an http:// or https:// URL")
	}
	if u.Host == "" {
		return fmt.Errorf("--url must include a host")
	}
	return nil
}

// probeEndpoint performs the actual health GET shared by the
// `health` and `healthcheck` subcommands and returns the probe's
// exit code: 0 on 2xx, 1 on anything else. label prefixes stderr
// messages so each subcommand's failures read the same way they
// did before the probe logic was factored out.
//
// The client deliberately does not follow redirects: /health is a
// fixed path on a service we control, and a 3xx would mean the
// indexer is misconfigured, not "the answer is elsewhere".
// Following a redirect could mask a real misconfiguration behind
// a successful probe.
func probeEndpoint(label, url string, timeout time.Duration) int {
	client := &http.Client{Timeout: timeout}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		// Only malformed URLs hit this path; both subcommands
		// validate their inputs first, so it's effectively
		// unreachable, but we still surface the error cleanly.
		fmt.Fprintf(os.Stderr, "%s: build request: %v\n", label, err)
		return 1
	}
	req.Header.Set("User-Agent", "sorotrail-"+label+"/1")

	resp, err := client.Do(req)
	if err != nil {
		// Connection refused (server not listening yet), timeout
		// (probe hung), DNS failure (broken --addr). All are
		// "not healthy right now" — same exit code, terse message
		// so probes surface a useful one-liner without an
		// avalanche of stack frames.
		fmt.Fprintf(os.Stderr, "%s: probe %s failed: %v\n", label, url, err)
		return 1
	}
	defer resp.Body.Close()
	// Drain a bounded prefix so the connection can be reused by
	// the keep-alive pool. /health responses are tiny (a small
	// JSON envelope) but we cap the read rather than reading
	// until EOF, so a malicious or misbehaving server can't pin
	// the process open after we've already decided the probe's
	// outcome from the status line.
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "%s: probe %s returned status %d\n",
			label, url, resp.StatusCode)
		return 1
	}
	return 0
}
