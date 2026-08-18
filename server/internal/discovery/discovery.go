// Package discovery advertises photod on the local network so the phone can
// find it without being told an address.
package discovery

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

// ServiceType must match NSBonjourServices in the app's Info.plist, or iOS
// blocks the query before it ever reaches the network.
const ServiceType = "_photobackup._tcp"

// Advertiser is a live mDNS registration.
type Advertiser struct {
	server *zeroconf.Server
	// Host is the name the SRV record points at, published with our own A
	// records.
	Host string
}

// Advertise publishes photod over mDNS and returns the registration.
//
// Callers should treat an error as a degradation, not a failure to start. Both
// macOS (mDNSResponder) and Fedora (Avahi) already hold port 5353, and if this
// responder cannot coexist with the system one, photod must still serve: the
// phone falls back to its last known good address, then to a manually entered
// one. On Fedora the sturdier alternative is a static Avahi service file in
// /etc/avahi/services plus MDNS_DISABLED=1 here.
//
// This uses RegisterProxy rather than Register for two reasons. Register derives
// the host name from os.Hostname() and appends the domain unconditionally, which
// on macOS turns "mac.local" into "mac.local.local" — a name nothing can
// resolve, and a difference in behaviour between the dev Mac and the Fedora
// archive machine. And publishing A records for the machine's real .local name
// would put us in a name fight with the responder that already owns it. Serving
// a name derived from the instance avoids both.
func Advertise(instance string, port int) (*Advertiser, error) {
	if instance == "" {
		instance = DefaultInstance()
	}
	ips, err := localAddresses()
	if err != nil {
		return nil, err
	}

	host := instance
	server, err := zeroconf.RegisterProxy(instance, ServiceType, "local.", port, host, ips, txtRecords(), nil)
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", ServiceType, err)
	}
	return &Advertiser{server: server, Host: host + ".local."}, nil
}

func (a *Advertiser) Shutdown() {
	if a != nil && a.server != nil {
		a.server.Shutdown()
	}
}

func txtRecords() []string {
	return []string{"path=/", "ver=1"}
}

// localAddresses collects the addresses worth publishing: every non-loopback
// address on an up, multicast-capable interface.
//
// IPv6 link-local addresses are skipped. They are only usable with a scope zone
// that an A/AAAA record cannot carry, and Phase 0 confirmed the phone dials the
// IPv4 address.
func localAddresses() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			ips = append(ips, ip.String())
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no usable local addresses to advertise")
	}
	return ips, nil
}

// DefaultInstance names the service after the machine, so several servers on one
// network stay distinguishable in a browse listing.
func DefaultInstance() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "photod"
	}
	// macOS reports a hostname that already ends in .local; the suffix belongs to
	// the domain, not the label.
	host = strings.TrimSuffix(host, ".local")
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "photod"
	}
	return "photod-" + host
}

// PortFrom extracts the TCP port from a listen address such as ":8787" or
// "0.0.0.0:8787", since mDNS advertises a port rather than an address.
func PortFrom(listenAddr string) (int, error) {
	_, rawPort, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return 0, fmt.Errorf("parse listen address %q: %w", listenAddr, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return 0, fmt.Errorf("listen port %q is not a number", rawPort)
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("listen port %d is out of range", port)
	}
	return port, nil
}
