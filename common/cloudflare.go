package common

import (
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// cloudflareCIDRs are Cloudflare's published edge ranges, taken from
// https://www.cloudflare.com/ips-v4 and https://www.cloudflare.com/ips-v6.
var cloudflareCIDRs = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
	"2400:cb00::/32",
	"2606:4700::/32",
	"2803:f800::/32",
	"2405:b500::/32",
	"2405:8100::/32",
	"2a06:98c0::/29",
	"2c0f:f248::/32",
}

var (
	cloudflareNetsOnce sync.Once
	cloudflareNets     []*net.IPNet
)

// isCloudflareIP reports whether ip belongs to a Cloudflare edge range. A CIDR
// that fails to parse is skipped, so a bad entry can only shrink the trusted
// set, never widen it.
func isCloudflareIP(ip net.IP) bool {
	cloudflareNetsOnce.Do(func() {
		for _, cidr := range cloudflareCIDRs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				SysError("invalid cloudflare cidr " + cidr + ": " + err.Error())
				continue
			}
			cloudflareNets = append(cloudflareNets, ipNet)
		}
	})
	for _, ipNet := range cloudflareNets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

// CloudflareClientIP returns the visitor IP from CF-Connecting-IP, but only when
// the direct TCP peer is a Cloudflare edge or a loopback/private/link-local hop,
// i.e. a local reverse proxy sitting between Cloudflare and this app. A public
// peer outside Cloudflare's ranges is never trusted; it could forge the header.
// It returns "" when the peer is untrusted or the header is missing/invalid.
func CloudflareClientIP(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	remoteIP := c.RemoteIP()
	peer := net.ParseIP(remoteIP)
	trusted := peer != nil && isCloudflareIP(peer)
	if !trusted {
		// netip, unlike net.ParseIP, accepts the zone Go puts on link-local
		// peers ("fe80::1%eth0"). Unmap so "::ffff:10.0.0.1" classifies as IPv4.
		if addr, err := netip.ParseAddr(remoteIP); err == nil {
			addr = addr.Unmap()
			trusted = addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()
		}
	}
	if !trusted {
		return ""
	}
	clientIP := net.ParseIP(strings.TrimSpace(c.GetHeader("CF-Connecting-IP")))
	if clientIP == nil {
		return ""
	}
	return clientIP.String()
}
