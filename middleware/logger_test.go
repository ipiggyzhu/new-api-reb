package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAccessLogFollowsWriterRotation pins the reason SetUpLogger installs its own
// output writer instead of letting gin default to gin.DefaultWriter: gin captures
// that writer once, when the middleware is built, while log rotation swaps
// gin.DefaultWriter and closes the file behind the old one. A captured writer
// would keep addressing a closed descriptor and drop every access log line after
// the first rotation, silently — the file keeps growing with [SYS] and [INFO]
// lines, so the loss is invisible until someone looks for a request that is not
// there.
func TestAccessLogFollowsWriterRotation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := gin.DefaultWriter
	t.Cleanup(func() { gin.DefaultWriter = original })

	beforeRotation := &bytes.Buffer{}
	gin.DefaultWriter = beforeRotation

	server := gin.New()
	SetUpLogger(server)
	server.GET("/probe", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	afterRotation := &bytes.Buffer{}
	gin.DefaultWriter = afterRotation

	request := httptest.NewRequest(http.MethodGet, "/probe", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code)

	assert.Contains(t, afterRotation.String(), "/probe",
		"the access log must land on the writer installed by the most recent rotation")
	assert.NotContains(t, beforeRotation.String(), "/probe",
		"a writer replaced by rotation must stop receiving access logs; in production it is a closed file")
}

// TestAccessLogKeepsRouteTagAndRequestId guards the log line's shape, which the
// operator greps and which the rotation fix had to preserve while moving from
// LoggerWithFormatter to LoggerWithConfig.
func TestAccessLogKeepsRouteTagAndRequestId(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := gin.DefaultWriter
	t.Cleanup(func() { gin.DefaultWriter = original })

	out := &bytes.Buffer{}
	gin.DefaultWriter = out

	server := gin.New()
	SetUpLogger(server)
	server.GET("/tagged", RouteTag("relay"), func(c *gin.Context) {
		c.Set("X-Oneapi-Request-Id", "req-1234")
		c.Status(http.StatusTeapot)
	})

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/tagged", nil))

	line := out.String()
	assert.Contains(t, line, "[GIN]")
	assert.Contains(t, line, "relay", "route tag must stay in the line")
	assert.Contains(t, line, "418", "status code must stay in the line")
	assert.Contains(t, line, "/tagged")
}

// TestAccessLogFallsBackToWebTag covers the untagged route branch, the only
// conditional in the formatter.
func TestAccessLogFallsBackToWebTag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := gin.DefaultWriter
	t.Cleanup(func() { gin.DefaultWriter = original })

	out := &bytes.Buffer{}
	gin.DefaultWriter = out

	server := gin.New()
	SetUpLogger(server)
	server.GET("/untagged", func(c *gin.Context) { c.Status(http.StatusOK) })

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/untagged", nil))

	assert.Contains(t, out.String(), "| web |", "a route without RouteTag must be logged as web")
}

// TestAccessLogRedactsQueryCredentials pins what the access log may keep from a
// query string. Gemini-style clients authenticate with ?key=sk-..., OAuth
// callbacks carry a one-time code and state, and the log file is read far more
// widely than the token table, so none of those may reach it, while ordinary
// parameters stay greppable and the handler still sees the original query.
func TestAccessLogRedactsQueryCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := gin.DefaultWriter
	t.Cleanup(func() { gin.DefaultWriter = original })

	cases := []struct {
		name        string
		target      string
		wantInLog   []string
		wantAbsent  []string
		handlerSees string
	}{
		{
			name:        "gemini api key in query",
			target:      "/v1beta/models/gemini-pro:streamGenerateContent?alt=sse&key=sk-live-secret",
			wantInLog:   []string{"/v1beta/models/gemini-pro:streamGenerateContent?", "alt=sse", "key=%2A%2A%2Amasked%2A%2A%2A"},
			wantAbsent:  []string{"sk-live-secret"},
			handlerSees: "sk-live-secret",
		},
		{
			name:        "oauth callback code and state",
			target:      "/api/oauth/github?code=one-time-code&state=csrf-state",
			wantInLog:   []string{"GET /api/oauth/github\n"},
			wantAbsent:  []string{"one-time-code", "csrf-state", "?"},
			handlerSees: "",
		},
		{
			name:        "malformed query is dropped, not half-masked",
			target:      "/v1beta/models/gemini-pro:generateContent?key=sk-live-secret%zz",
			wantInLog:   []string{"GET /v1beta/models/gemini-pro:generateContent\n"},
			wantAbsent:  []string{"sk-live-secret", "?"},
			handlerSees: "",
		},
		{
			name:        "ordinary query stays readable",
			target:      "/api/log/self?p=2&page_size=10",
			wantInLog:   []string{"/api/log/self?p=2&page_size=10"},
			handlerSees: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			gin.DefaultWriter = out

			var seenKey string
			server := gin.New()
			SetUpLogger(server)
			server.NoRoute(func(c *gin.Context) {
				seenKey = c.Query("key")
				c.Status(http.StatusNoContent)
			})

			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.target, nil))
			require.Equal(t, http.StatusNoContent, recorder.Code)

			line := out.String()
			for _, want := range tc.wantInLog {
				assert.Contains(t, line, want)
			}
			// The redaction must not reach the request itself.
			if tc.handlerSees != "" {
				assert.Equal(t, tc.handlerSees, seenKey)
			}
			_, logged, _ := strings.Cut(line, "| GET ")
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, logged, absent)
			}
		})
	}
}
