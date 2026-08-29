package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrustedProxyList(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	assert.Equal(t,
		[]string{"127.0.0.0/8", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"},
		trustedProxyList())

	t.Setenv("TRUSTED_PROXIES", " 203.0.113.1/32 , 198.51.100.0/24 ,")
	assert.Equal(t, []string{"203.0.113.1/32", "198.51.100.0/24"}, trustedProxyList())
}

func TestClientIPHonoursOnlyTrustedProxies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("TRUSTED_PROXIES", "")

	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(trustedProxyList()))
	engine.GET("/ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	tests := []struct {
		name         string
		remoteAddr   string
		forwardedFor string
		wantClientIP string
	}{
		{
			name:         "trusted docker bridge peer forwards client ip",
			remoteAddr:   "172.17.0.1:52341",
			forwardedFor: "203.0.113.10",
			wantClientIP: "203.0.113.10",
		},
		{
			name:         "public peer cannot forge client ip",
			remoteAddr:   "198.51.100.7:52341",
			forwardedFor: "203.0.113.10",
			wantClientIP: "198.51.100.7",
		},
		{
			name:         "forged prefix is ignored when proxy appends real ip",
			remoteAddr:   "172.17.0.1:52341",
			forwardedFor: "203.0.113.10, 198.51.100.7",
			wantClientIP: "198.51.100.7",
		},
		{
			name:         "direct connection without header",
			remoteAddr:   "198.51.100.7:52341",
			wantClientIP: "198.51.100.7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.forwardedFor)
			}
			engine.ServeHTTP(w, req)
			assert.Equal(t, tt.wantClientIP, w.Body.String())
		})
	}
}
