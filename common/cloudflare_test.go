package common

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloudflareClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.cfHeader != "" {
				req.Header.Set("CF-Connecting-IP", tt.cfHeader)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			require.NotNil(t, c.Request)

			assert.Equal(t, tt.want, CloudflareClientIP(c))
		})
	}
}
