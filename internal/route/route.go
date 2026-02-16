// Package route provides OS-specific add/remove of a static host route
// so that traffic to a given destination goes via a specific interface.
package route

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Manager adds and removes a static route for a destination IP via an interface or gateway.
// Typically requires root (or CAP_NET_ADMIN on Linux).
type Manager interface {
	// Add adds a route so that destIP is reached via the given interface or nexthop.
	// If viaNexthop is non-empty, it is used as the gateway (for when the default route
	// is via another interface). Otherwise the default gateway for viaInterface is used.
	Add(ctx context.Context, destIP, viaInterface, viaNexthop string) error
	// Remove removes the route for destIP that was added by Add.
	Remove(ctx context.Context, destIP string) error
}

// NewManager returns a Manager for the current OS, or nil and an error if unsupported.
func NewManager() (Manager, error) {
	switch runtime.GOOS {
	case "linux":
		return &linuxManager{}, nil
	case "darwin":
		return &darwinManager{}, nil
	default:
		return nil, fmt.Errorf("route manager not implemented for %s", runtime.GOOS)
	}
}

type linuxManager struct{}

// parseLinuxDefaultRoute parses the first line of "ip route show default" output.
// Line looks like: "default via 192.168.1.1 dev eth0 ..."
func parseLinuxDefaultRoute(line string) (gateway, iface string, err error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", fmt.Errorf("empty default route line")
	}
	fields := strings.Fields(line)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "via" && i+1 < len(fields) {
			gateway = fields[i+1]
		}
		if fields[i] == "dev" && i+1 < len(fields) {
			iface = fields[i+1]
			break
		}
	}
	if gateway == "" || iface == "" {
		return "", "", fmt.Errorf("could not parse default route: %q", line)
	}
	return gateway, iface, nil
}

// getLinuxDefaultRoute returns the default gateway and interface from "ip route show default".
func getLinuxDefaultRoute(ctx context.Context) (gateway, iface string, err error) {
	cmd := exec.CommandContext(ctx, "ip", "route", "show", "default")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("ip route show default: %w: %s", err, out)
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return parseLinuxDefaultRoute(line)
}

func (m *linuxManager) Add(ctx context.Context, destIP, viaInterface, viaNexthop string) error {
	var gateway string
	if viaNexthop != "" {
		gateway = viaNexthop
	} else {
		gw, defaultIface, err := getLinuxDefaultRoute(ctx)
		if err != nil {
			return err
		}
		if defaultIface != viaInterface {
			return fmt.Errorf("default route is via %s, not %s; use -route-via-nexthop to specify the gateway for %s",
				defaultIface, viaInterface, viaInterface)
		}
		gateway = gw
	}
	// ip route add DEST via GATEWAY [dev IFACE] — traffic goes to gateway
	args := []string{"route", "add", destIP, "via", gateway}
	if viaInterface != "" && viaNexthop == "" {
		args = append(args, "dev", viaInterface)
	}
	cmd := exec.CommandContext(ctx, "ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route add %s via %s: %w: %s", destIP, gateway, err, out)
	}
	return nil
}

func (m *linuxManager) Remove(ctx context.Context, destIP string) error {
	cmd := exec.CommandContext(ctx, "ip", "route", "del", destIP)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip route del %s: %w: %s", destIP, err, out)
	}
	return nil
}

type darwinManager struct{}

// parseDarwinDefaultRoute parses output of "route -n get default" (macOS).
func parseDarwinDefaultRoute(output string) (gateway, iface string, err error) {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			gateway = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
		if strings.HasPrefix(line, "interface:") {
			iface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	if gateway == "" {
		return "", "", fmt.Errorf("no gateway in route get default")
	}
	return gateway, iface, nil
}

// getDefaultGateway returns the default gateway IP and interface from "route -n get default".
func getDefaultGateway(ctx context.Context) (gateway, iface string, err error) {
	cmd := exec.CommandContext(ctx, "route", "-n", "get", "default")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("route get default: %w: %s", err, out)
	}
	return parseDarwinDefaultRoute(string(out))
}

func (m *darwinManager) Add(ctx context.Context, destIP, viaInterface, viaNexthop string) error {
	var gateway string
	if viaNexthop != "" {
		gateway = viaNexthop
	} else {
		gw, defaultIface, err := getDefaultGateway(ctx)
		if err != nil {
			return err
		}
		if defaultIface != viaInterface {
			return fmt.Errorf("default route is via %s, not %s; use -route-via-nexthop to specify the gateway for %s",
				defaultIface, viaInterface, viaInterface)
		}
		gateway = gw
	}
	// route add -host DEST GATEWAY — traffic to DEST goes to GATEWAY, which forwards to server
	cmd := exec.CommandContext(ctx, "route", "add", "-host", destIP, gateway)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add -host %s %s: %w: %s", destIP, gateway, err, out)
	}
	return nil
}

func (m *darwinManager) Remove(ctx context.Context, destIP string) error {
	cmd := exec.CommandContext(ctx, "route", "delete", destIP)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("route delete %s: %w: %s", destIP, err, out)
	}
	return nil
}

// ErrUnsupported is returned when the OS is not supported.
var ErrUnsupported = errors.New("route manager not supported on this OS")

// InterfaceForNexthop returns the interface the kernel uses to reach the given nexthop (gateway) IP.
func InterfaceForNexthop(ctx context.Context, nexthop string) (iface string, err error) {
	switch runtime.GOOS {
	case "linux":
		return interfaceForNexthopLinux(ctx, nexthop)
	case "darwin":
		return interfaceForNexthopDarwin(ctx, nexthop)
	default:
		return "", ErrUnsupported
	}
}

// interfaceForNexthopLinux runs "ip route get <nexthop>" and parses "dev IFACE".
func interfaceForNexthopLinux(ctx context.Context, nexthop string) (string, error) {
	cmd := exec.CommandContext(ctx, "ip", "route", "get", nexthop)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ip route get %s: %w: %s", nexthop, err, out)
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	fields := strings.Fields(line)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("could not parse interface from ip route get %s: %q", nexthop, line)
}

// interfaceForNexthopDarwin runs "route -n get <nexthop>" and parses "interface: IFACE".
func interfaceForNexthopDarwin(ctx context.Context, nexthop string) (string, error) {
	cmd := exec.CommandContext(ctx, "route", "-n", "get", nexthop)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("route get %s: %w: %s", nexthop, err, out)
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "interface:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "interface:")), nil
		}
	}
	return "", fmt.Errorf("could not parse interface from route get %s", nexthop)
}
