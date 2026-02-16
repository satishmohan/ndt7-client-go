// ndt7-client is the ndt7 command line client.
//
// Usage:
//
//	ndt7-client [flags]
//
// The `-format` flag defines how the output should be emitter. Possible
// values are "human", which is the default, and "json", where each message
// is a valid JSON object.
//
// The (DEPRECATED) `-batch` flag is equivalent to `-format json`, and the
// latter should be used instead.
//
// The default behavior is for ndt7-client to discover a suitable server using
// Measurement Lab's locate service. This behavior may be overridden using
// either the `-server` or `-service-url` flags.
//
// The `-server <name>` flag specifies the server `name` for performing
// the ndt7 test. This option overrides `-service-url`.
//
// The `-service-url <url>` flag specifies a complete URL that specifies the
// scheme (e.g. "ws"), server name and port, protocol (e.g. /ndt/v7/download),
// and HTTP parameters. By default, upload and download measurements are run
// automatically. The `-service-url` specifies only one measurement direction.
//
// The `-no-verify` flag allows to skip TLS certificate verification.
//
// The `-scheme <scheme>` flag allows to override the default scheme, i.e.,
// "wss", with another scheme. The only other supported scheme is "ws"
// and causes ndt7 to run unencrypted.
//
// The `-timeout <string>` flag specifies the time after which the
// whole test is interrupted. The `<string>` is a string suitable to
// be passed to time.ParseDuration, e.g., "15s". The default is a large
// enough value that should be suitable for common conditions.
//
// The `-upload` and `-download` flags are boolean options that default to true,
// but may be set to false on the command line to run only upload or only
// download.
//
// The `-profile` flag defines the file where to write a CPU profile
// that later you can pass to `go tool pprof`. See https://blog.golang.org/pprof.
//
// The `-list-servers` flag fetches the list of nearest ndt7 servers from Locate,
// prints them (one per line with index and hostname), and exits.
//
// The `-server-index <n>` flag selects the n-th server from the Locate list when
// not using `-server` (e.g. when using `-route-via-interface` without `-server`).
//
// The `-route-via-interface <iface>` flag adds a static route via the default
// gateway for that interface; use when your default route is already via that interface.
// The `-route-via-nexthop <gateway>` flag adds a static route via the given gateway IP;
// use when the default route is via another interface (e.g. traffic via eth1's gateway).
// Both require root (or CAP_NET_ADMIN on Linux). Supported on Linux and macOS.
//
// When using `-server` with a short hostname (no dot), TLS ServerName is set to
// <server>.measurementlab.net so certificate verification passes against M-Lab certs.
//
// Additionally, passing any unrecognized flag, such as `-help`, will
// cause ndt7-client to print a brief help message.
//
// JSON events emitted when -format=json
//
// This section describes the events emitted when using the json output format.
// The code will always emit a single event per line.
//
// When the download test starts, this event is emitted:
//
//	{"Key":"starting","Value":{"Test":"download"}}
//
// After this event is emitted, we discover the server to use (unless it
// has been configured by the user) and we connect to it. If any of these
// operations fail, this event is emitted:
//
//	{"Key":"error","Value":{"Failure":"<failure>","Test":"download"}}
//
// where `<failure>` is the error that occurred serialized as string. In
// case of failure, the test is over and the next event to be emitted is
// `"complete"`
//
// Otherwise, the download test starts and we see the following event:
//
//	{"Key":"connected","Value":{"Server":"<server>","Test":"download"}}
//
// where `<server>` is the FQDN of the server we're using. Then there
// are zero or more events like:
//
//	{"Key": "measurement","Value": <value>}
//
// where `<value>` is a serialized spec.Measurement struct.
//
// Finally, this event is always emitted at the end of the test:
//
//	{"Key":"complete","Value":{"Test":"download"}}
//
// The upload test is like the download test, except for the
// value of the `"Test"` key.
//
// # Exit code
//
// This tool exits with zero on success, nonzero on failure. Under
// some severe internal error conditions, this tool will exit using
// a nonzero exit code without being able to print a diagnostic
// message explaining the error that occurred. In all other cases,
// checking the output should help to understand the error cause.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/m-lab/go/flagx"
	"github.com/m-lab/go/rtx"
	"github.com/m-lab/locate/api/locate"
	v2 "github.com/m-lab/locate/api/v2"
	"github.com/m-lab/ndt7-client-go"
	"github.com/m-lab/ndt7-client-go/internal/emitter"
	"github.com/m-lab/ndt7-client-go/internal/params"
	"github.com/m-lab/ndt7-client-go/internal/route"
	"github.com/m-lab/ndt7-client-go/internal/runner"
	"golang.org/x/sys/cpu"
)

const (
	defaultTimeout = 55 * time.Second
)

var (
	ClientVersion = "0.10.1"

	fset = flag.NewFlagSet("ndt7-client", flag.ExitOnError)

	flagProfile = fset.String("profile", "",
		"file where to store pprof profile (see https://blog.golang.org/pprof)")

	flagScheme = flagx.Enum{
		Options: []string{"wss", "ws"},
		Value:   defaultSchemeForArch(),
	}

	flagFormat = flagx.Enum{
		Options: []string{"human", "json"},
		Value:   "human",
	}

	flagBatch = fset.Bool("batch", false, "emit JSON events on stdout "+
		"(DEPRECATED, please use -format=json)")
	flagNoVerify   = fset.Bool("no-verify", false, "skip TLS certificate verification")
	flagServer     = fset.String("server", "", "optional ndt7 server hostname")
	flagClientName = fset.String("client-name", "ndt7-client-go-cmd", "The client_name reported to Locate and ndt-server")
	flagTimeout    = fset.Duration(
		"timeout", defaultTimeout, "time after which the test is aborted")
	flagQuiet    = fset.Bool("quiet", false, "emit summary and errors only")
	flagService  = flagx.URL{}
	flagUpload   = fset.Bool("upload", true, "perform upload measurement")
	flagDownload = fset.Bool("download", true, "perform download measurement")

	flagLocateToken = fset.String(
		"locate.token",
		"",
		"Optional short-lived JWT token for registered integrator access. Integrators "+
			"typically obtain this token by interacting with their own backend.",
	)
	flagLocateURL = fset.String(
		"locate.url",
		"",
		"Override the default locate URL. If the URL is empty, we use a suitable "+
			"https://locate.measurementlab.net/{path} URL where the {path} depends on "+
			"whether you specified -locate.token or not.")

	flagListServers       = fset.Bool("list-servers", false, "fetch and print nearest ndt7 servers from Locate, then exit")
	flagServerIndex       = fset.Int("server-index", 0, "when not using -server, index of the server to use from Locate list (use with -list-servers to see indices)")
	flagRouteViaInterface = fset.String("route-via-interface", "", "add a static route via this interface (uses default gateway for that interface); requires root")
	flagRouteViaNexthop   = fset.String("route-via-nexthop", "", "add a static route via this gateway (nexthop) IP; use when default route is via another interface; requires root")
)

func init() {
	fset.Var(
		&flagScheme,
		"scheme",
		`WebSocket scheme to use: either "wss" or "ws"`,
	)
	fset.Var(
		&flagFormat,
		"format",
		"output format to use: 'human' or 'json' for batch processing",
	)
	fset.Var(
		&flagService,
		"service-url",
		"Service URL specifies target hostname and other URL fields like access token. Overrides -server.",
	)
}

// defaultSchemeForArch returns the default WebSocket scheme to use, depending
// on the architecture we are running on. A CPU without native AES instructions
// will perform poorly if TLS is enabled.
func defaultSchemeForArch() string {
	if cpu.ARM64.HasAES || cpu.ARM.HasAES || cpu.X86.HasAES {
		return "wss"
	}
	return "ws"
}

var (
	osExit = os.Exit
	osArgs = os.Args

	// serverHostOverride is set when we resolve the server from Locate (e.g. -server-index)
	// so that clientFactory uses it instead of -server.
	serverHostOverride string
	// serverAccessToken is the access_token from the Locate target URL when using serverHostOverride.
	serverAccessToken string
)

func main() {
	_ = fset.Parse(osArgs[1:]) // we're using [flag.ExitOnError]
	rtx.Must(flagx.ArgsFromEnvWithLog(fset, false), "failed to parse flags")

	if *flagProfile != "" {
		log.Printf("warning: using -profile will reduce the performance")
		fp, err := os.Create(*flagProfile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(fp)
		defer pprof.StopCPUProfile()
	}

	// If a service URL is given, then only one direction is possible.
	if flagService.URL != nil && strings.Contains(flagService.URL.Path, params.DownloadURLPath) {
		*flagUpload = false
		*flagDownload = true
	} else if flagService.URL != nil && strings.Contains(flagService.URL.Path, params.UploadURLPath) {
		*flagUpload = true
		*flagDownload = false
	} else if flagService.URL != nil {
		fmt.Println("WARNING: ignoring unsupported service url")
		flagService.URL = nil
	}

	var e emitter.Emitter

	// If -batch, force -format=json.
	if *flagBatch || flagFormat.Value == "json" {
		e = emitter.NewJSON(os.Stdout)
	} else {
		e = emitter.NewHumanReadable()
	}
	if *flagQuiet {
		e = emitter.NewQuiet(e)
	}

	// -list-servers: fetch from Locate, print, exit.
	if *flagListServers {
		listServersAndExit()
	}

	// Determine which server to use (for -route-via-interface we need it upfront).
	serverHost, serverToken, err := getServerHost()
	if err != nil {
		log.Fatalf("server selection: %v", err)
	}

	// -route-via-interface or -route-via-nexthop: add static route for each server IP, run test, remove routes.
	// Route removal must be done explicitly before os.Exit() because os.Exit() skips defers.
	var routeManager route.Manager
	var routeCtx context.Context
	var addedRouteIPs []string
	routeViaInterface := *flagRouteViaInterface
	routeViaNexthop := *flagRouteViaNexthop
	if routeViaInterface != "" || routeViaNexthop != "" {
		if serverHost == "" {
			log.Fatal("when using -route-via-interface or -route-via-nexthop, -server or -server-index (with Locate) must identify the server")
		}
		var err error
		routeManager, err = route.NewManager()
		if err != nil {
			log.Fatalf("route: %v", err)
		}
		destIPs, err := resolveHostToIPs(serverHost)
		if err != nil {
			log.Fatalf("resolve server %q: %v", serverHost, err)
		}
		routeCtx = context.Background()
		var derivedIface string
		if routeViaNexthop != "" {
			var err error
			derivedIface, err = route.InterfaceForNexthop(routeCtx, routeViaNexthop)
			if err != nil {
				log.Printf("(could not derive interface for nexthop %s: %v)", routeViaNexthop, err)
			}
		}
		for _, destIP := range destIPs {
			if err := routeManager.Add(routeCtx, destIP, routeViaInterface, routeViaNexthop); err != nil {
				log.Fatalf("route add %s: %v (try running as root)", destIP, err)
			}
			if routeViaNexthop != "" {
				if derivedIface != "" {
					log.Printf("Added route: %s via nexthop %s (interface %s)", destIP, routeViaNexthop, derivedIface)
				} else {
					log.Printf("Added route: %s via nexthop %s", destIP, routeViaNexthop)
				}
			} else {
				log.Printf("Added route: %s via %s", destIP, routeViaInterface)
			}
			addedRouteIPs = append(addedRouteIPs, destIP)
		}
		if routeViaNexthop != "" {
			if derivedIface != "" {
				log.Printf("Routing traffic to %s via nexthop %s (interface %s) (static route active)", serverHost, routeViaNexthop, derivedIface)
			} else {
				log.Printf("Routing traffic to %s via nexthop %s (static route active)", serverHost, routeViaNexthop)
			}
		} else {
			log.Printf("Routing traffic to %s via interface %s (static route active)", serverHost, routeViaInterface)
		}
	}

	// If we resolved server from Locate, tell clientFactory to use it (and the access token).
	if serverHost != "" && *flagServer == "" {
		serverHostOverride = serverHost
		serverAccessToken = serverToken
	}

	r := runner.New(
		runner.RunnerOptions{
			Download:      *flagDownload,
			Upload:        *flagUpload,
			Timeout:       *flagTimeout,
			ClientFactory: clientFactory,
		},
		e,
		nil)

	errs := r.RunTestsOnce()

	// Remove routes before exiting; os.Exit() does not run defers.
	for _, destIP := range addedRouteIPs {
		if err := routeManager.Remove(routeCtx, destIP); err != nil {
			log.Printf("warning: route remove %s: %v", destIP, err)
		} else {
			log.Printf("Removed route: %s", destIP)
		}
	}

	osExit(len(errs))
}

// clientFactory constructs a [*ndt7.Client] given command line flags values
func clientFactory() *ndt7.Client {
	parsedLocateURL := getLocateURL()

	c := ndt7.NewClient(*flagClientName, ClientVersion)

	c.ServiceURL = flagService.URL
	if serverHostOverride != "" {
		c.Server = serverHostOverride
		c.AccessToken = serverAccessToken
	} else {
		c.Server = *flagServer
	}
	c.Scheme = flagScheme.Value
	// When using route flags, force wss (port 443) so the connection
	// uses the standard TLS port; ws (port 80) often times out or is blocked.
	if (*flagRouteViaInterface != "" || *flagRouteViaNexthop != "") && c.Scheme == "ws" {
		c.Scheme = "wss"
	}

	// TLS: when -server is a short hostname (no dot), M-Lab certs are for FQDN
	// (e.g. ndt-mlab1-lga0t.measurementlab.net). Set ServerName so verification passes.
	tlsConfig := &tls.Config{InsecureSkipVerify: *flagNoVerify}
	if c.Server != "" && !strings.Contains(c.Server, ".") {
		tlsConfig.ServerName = c.Server + ".measurementlab.net"
	}
	c.Dialer.TLSClientConfig = tlsConfig

	// When using route flags, we only add routes for IPv4. Force the
	// dialer to use IPv4 so the connection uses the IP we routed.
	if *flagRouteViaInterface != "" || *flagRouteViaNexthop != "" {
		c.Dialer.NetDialContext = dialContextIPv4
	}

	loc := locate.NewClient(ndt7.MakeUserAgent(c.ClientName, c.ClientVersion))
	loc.BaseURL = parsedLocateURL
	loc.Authorization = *flagLocateToken
	c.Locate = loc

	return c
}

// dialContextIPv4 resolves addr (host:port) to an IPv4 address and dials tcp4.
// Used with -route-via-interface so the connection uses the IPv4 address we routed.
func dialContextIPv4(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("unsupported network %q", network)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPv4 address for %s", host)
	}
	// Try each IPv4 in order (same as standard resolver behavior).
	var lastErr error
	for _, ip := range ips {
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// getLocateURL returns the parsed Locate base URL from flags.
func getLocateURL() *url.URL {
	locateURL := *flagLocateURL
	if locateURL == "" {
		locateURL = "https://locate.measurementlab.net"
		if *flagLocateToken != "" {
			locateURL += "/v2/priority/nearest"
		} else {
			locateURL += "/v2/nearest"
		}
	}
	parsed, err := url.Parse(locateURL)
	rtx.Must(err, "failed to parse locate URL %q", locateURL)
	return parsed
}

// getServerHost returns the server hostname and access token: -server if set (token empty),
// otherwise the host and token from the Locate target at -server-index.
// Empty host when using Locate without route/list (client will discover on connect).
func getServerHost() (host, accessToken string, err error) {
	if *flagServer != "" {
		return *flagServer, "", nil
	}
	if *flagRouteViaInterface == "" && *flagRouteViaNexthop == "" && !*flagListServers {
		return "", "", nil
	}
	// Need to fetch from Locate to pick by index or list.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	loc := locate.NewClient(ndt7.MakeUserAgent(*flagClientName, ClientVersion))
	loc.BaseURL = getLocateURL()
	loc.Authorization = *flagLocateToken
	targets, err := loc.Nearest(ctx, "ndt/ndt7")
	if err != nil {
		return "", "", fmt.Errorf("locate: %w", err)
	}
	if len(targets) == 0 {
		return "", "", fmt.Errorf("locate returned no targets")
	}
	idx := *flagServerIndex
	if idx < 0 || idx >= len(targets) {
		return "", "", fmt.Errorf("server-index %d out of range (0..%d)", idx, len(targets)-1)
	}
	k := flagScheme.Value + "://" + params.DownloadURLPath
	rawURL, ok := targets[idx].URLs[k]
	if !ok {
		return "", "", fmt.Errorf("target has no URL for %s", k)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse target URL: %w", err)
	}
	return u.Hostname(), u.Query().Get("access_token"), nil
}

// listServersAndExit fetches targets from Locate, prints them, and exits.
func listServersAndExit() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	loc := locate.NewClient(ndt7.MakeUserAgent(*flagClientName, ClientVersion))
	loc.BaseURL = getLocateURL()
	loc.Authorization = *flagLocateToken
	targets, err := loc.Nearest(ctx, "ndt/ndt7")
	if err != nil {
		log.Fatalf("list-servers: %v", err)
	}
	for i, t := range targets {
		host := targetHost(t)
		if host != "" {
			fmt.Printf("%d\t%s\n", i, host)
		}
	}
	os.Exit(0)
}

func targetHost(t v2.Target) string {
	k := flagScheme.Value + "://" + params.DownloadURLPath
	rawURL, ok := t.URLs[k]
	if !ok {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// resolveHostToIPs returns IPv4 addresses for the host for use when adding routes.
// Only IPv4 is used because macOS "route add -host" does not accept IPv6 addresses.
func resolveHostToIPs(host string) ([]string, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			out = append(out, ip4.String())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no IPv4 address for %s", host)
	}
	return out, nil
}
