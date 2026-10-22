package daemon

import (
	"net"
	"sort"
	"strings"
)

// netSignature summarizes this machine's routable addresses (no loopback,
// no link-local: those come and go with every container's veth). A change
// means the network moved under us, the moment to retry unreachable peers.
func netSignature() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, ipn.String())
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
