package route

import (
	"context"
	"os"
	"runtime"
	"testing"
)

func TestParseLinuxDefaultRoute(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantGW    string
		wantIface string
		wantErr   bool
	}{
		{
			name:      "standard",
			line:      "default via 192.168.1.1 dev eth0",
			wantGW:    "192.168.1.1",
			wantIface: "eth0",
		},
		{
			name:      "with proto",
			line:      "default via 10.0.0.1 dev eth0 proto static",
			wantGW:    "10.0.0.1",
			wantIface: "eth0",
		},
		{
			name:      "multiple metrics",
			line:      "default via 172.16.0.1 dev enp0s3 proto dhcp metric 100",
			wantGW:    "172.16.0.1",
			wantIface: "enp0s3",
		},
		{
			name:    "empty",
			line:    "",
			wantErr: true,
		},
		{
			name:    "no via",
			line:    "default dev eth0",
			wantErr: true,
		},
		{
			name:    "no dev",
			line:    "default via 192.168.1.1",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotGW, gotIface, err := parseLinuxDefaultRoute(tt.line)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseLinuxDefaultRoute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if gotGW != tt.wantGW {
					t.Errorf("parseLinuxDefaultRoute() gateway = %q, want %q", gotGW, tt.wantGW)
				}
				if gotIface != tt.wantIface {
					t.Errorf("parseLinuxDefaultRoute() iface = %q, want %q", gotIface, tt.wantIface)
				}
			}
		})
	}
}

func TestParseDarwinDefaultRoute(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantGW    string
		wantIface string
		wantErr   bool
	}{
		{
			name: "standard",
			output: `   route to: default
   destination: default
        mask: default
     gateway: 172.16.0.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING>
 recvpipe  sendpipe  ssthresh  rtt,msec  rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1500         0`,
			wantGW:    "172.16.0.1",
			wantIface: "en0",
		},
		{
			name: "minimal",
			output: `gateway: 192.168.1.1
interface: eth0`,
			wantGW:    "192.168.1.1",
			wantIface: "eth0",
		},
		{
			name:    "no gateway",
			output:  "interface: en0\n",
			wantErr: true,
		},
		{
			name:    "empty",
			output:  "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotGW, gotIface, err := parseDarwinDefaultRoute(tt.output)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseDarwinDefaultRoute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if gotGW != tt.wantGW {
					t.Errorf("parseDarwinDefaultRoute() gateway = %q, want %q", gotGW, tt.wantGW)
				}
				if gotIface != tt.wantIface {
					t.Errorf("parseDarwinDefaultRoute() iface = %q, want %q", gotIface, tt.wantIface)
				}
			}
		})
	}
}

func TestNewManager(t *testing.T) {
	mgr, err := NewManager()
	switch runtime.GOOS {
	case "linux", "darwin":
		if err != nil {
			t.Fatalf("NewManager() on %s: %v", runtime.GOOS, err)
		}
		if mgr == nil {
			t.Fatal("NewManager() returned nil manager")
		}
	default:
		if err == nil {
			t.Fatal("NewManager() expected error on unsupported OS")
		}
		if mgr != nil {
			t.Fatal("NewManager() expected nil manager on unsupported OS")
		}
	}
}

// TestLinuxAddRemoveIntegration runs real route add/remove on Linux when
// ROUTE_INTEGRATION=1 and the process has root. Use a TEST-NET address (RFC 5737)
// that is not routed on the public internet.
func TestLinuxAddRemoveIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("integration test only on Linux")
	}
	if os.Getenv("ROUTE_INTEGRATION") != "1" {
		t.Skip("set ROUTE_INTEGRATION=1 to run (requires root)")
	}
	// 203.0.113.1 is in TEST-NET-3, safe for documentation/testing
	const testDest = "203.0.113.1"

	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx := context.Background()

	// Get default interface so we can pass it (we're testing that Add/Remove work)
	_, iface, err := getLinuxDefaultRoute(ctx)
	if err != nil {
		t.Fatalf("getLinuxDefaultRoute: %v (run with network)", err)
	}
	t.Logf("default route dev %s", iface)

	if err := mgr.Add(ctx, testDest, iface, ""); err != nil {
		t.Fatalf("Add: %v (need root? run: sudo ROUTE_INTEGRATION=1 go test -v -run TestLinuxAddRemove)", err)
	}
	t.Logf("Added route to %s via %s", testDest, iface)

	defer func() {
		if err := mgr.Remove(ctx, testDest); err != nil {
			t.Errorf("Remove: %v", err)
		} else {
			t.Logf("Removed route to %s", testDest)
		}
	}()

	// Route is active; defer above will remove it.
}
