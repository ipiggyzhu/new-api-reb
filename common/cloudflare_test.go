package common

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func cloudflareTestContext(remoteAddr, cfHeader string) *gin.Context {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	if cfHeader != "" {
		req.Header.Set("CF-Connecting-IP", cfHeader)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}

func TestCloudflareClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		cfHeader   string
		want       string
	}{
		{
			name:       "cloudflare v4 peer forwards visitor ip",
			remoteAddr: "172.70.1.2:443",
			cfHeader:   "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "non cloudflare public peer cannot forge header",
			remoteAddr: "198.51.100.9:443",
			cfHeader:   "203.0.113.7",
			want:       "",
		},
		{
			name:       "cloudflare peer without header falls back",
			remoteAddr: "104.16.0.1:443",
			want:       "",
		},
		{
			name:       "cloudflare peer with invalid header is ignored",
			remoteAddr: "104.16.0.1:443",
			cfHeader:   "not-an-ip",
			want:       "",
		},
		{
			name:       "cloudflare v6 peer forwards v6 visitor ip",
			remoteAddr: "[2606:4700::1]:443",
			cfHeader:   "2001:db8::42",
			want:       "2001:db8::42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cloudflareTestContext(tt.remoteAddr, tt.cfHeader)

			assert.Equal(t, tt.want, CloudflareClientIP(c))
		})
	}
}

// Behind Cloudflare -> local reverse proxy -> app the direct peer is the proxy on
// loopback or a private network, so trusting Cloudflare edges alone logged the
// edge instead of the visitor. That local hop is trusted with no site option.
// The peer is normalised before it is classified, so IPv6 local forms and
// IPv4-mapped IPv6 land in the same class as their plain IPv4 equivalents. A
// public peer outside Cloudflare's ranges, including addresses right next to the
// trusted ranges, must never pick the client IP, and a header that is not a
// single IP is never used.
func TestCloudflareClientIPPeerTrustMatrix(t *testing.T) {
	const visitor = "203.0.113.7"

	peers := []struct {
		name       string
		remoteAddr string
		trusted    bool
	}{
		{name: "cloudflare v4 range", remoteAddr: "172.70.1.2:443", trusted: true},
		{name: "cloudflare v6 range", remoteAddr: "[2606:4700::1]:443", trusted: true},
		{name: "ipv4 mapped cloudflare", remoteAddr: "[::ffff:172.70.1.2]:443", trusted: true},
		{name: "ipv4 loopback", remoteAddr: "127.0.0.1:51234", trusted: true},
		{name: "ipv6 loopback", remoteAddr: "[::1]:51234", trusted: true},
		{name: "ipv4 mapped loopback", remoteAddr: "[::ffff:127.0.0.1]:51234", trusted: true},
		{name: "rfc1918 10/8", remoteAddr: "10.12.0.5:51234", trusted: true},
		{name: "rfc1918 172.16/12", remoteAddr: "172.31.255.254:51234", trusted: true},
		{name: "rfc1918 192.168/16", remoteAddr: "192.168.1.20:51234", trusted: true},
		{name: "ipv4 mapped private", remoteAddr: "[::ffff:10.0.0.1]:51234", trusted: true},
		{name: "ipv6 unique local fc half", remoteAddr: "[fc00::1]:51234", trusted: true},
		{name: "ipv6 unique local fd half", remoteAddr: "[fd12:3456::8]:51234", trusted: true},
		{name: "ipv6 link local", remoteAddr: "[fe80::1]:51234", trusted: true},
		// Go reports link-local peers with their interface zone.
		{name: "ipv6 link local with zone", remoteAddr: "[fe80::1%eth0]:51234", trusted: true},
		{name: "ipv6 link local upper edge", remoteAddr: "[febf::1]:51234", trusted: true},
		{name: "ipv4 link local", remoteAddr: "169.254.10.20:51234", trusted: true},
		{name: "ipv4 mapped link local", remoteAddr: "[::ffff:169.254.10.20]:51234", trusted: true},
		{name: "public ipv4", remoteAddr: "198.51.100.9:443", trusted: false},
		{name: "public ipv6", remoteAddr: "[2001:db8::1]:443", trusted: false},
		{name: "ipv4 mapped public", remoteAddr: "[::ffff:198.51.100.9]:443", trusted: false},
		{name: "carrier grade nat is not rfc1918", remoteAddr: "100.64.0.1:51234", trusted: false},
		{name: "just past 172.16/12", remoteAddr: "172.32.0.1:51234", trusted: false},
		{name: "just past ipv6 link local", remoteAddr: "[fec0::1]:51234", trusted: false},
		{name: "unparseable peer", remoteAddr: "not-a-peer", trusted: false},
	}
	headers := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "valid header", value: visitor, valid: true},
		{name: "missing header", value: "", valid: false},
		// What a proxy that appends instead of overwriting would produce.
		{name: "malformed header", value: visitor + ", 104.23.160.12", valid: false},
	}

	for _, peer := range peers {
		for _, header := range headers {
			want := ""
			if peer.trusted && header.valid {
				want = visitor
			}

			t.Run(peer.name+"/"+header.name, func(t *testing.T) {
				c := cloudflareTestContext(peer.remoteAddr, header.value)

				assert.Equal(t, want, CloudflareClientIP(c))
			})
		}
	}
}
