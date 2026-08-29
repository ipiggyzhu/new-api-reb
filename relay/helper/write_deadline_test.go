package helper

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureErrorLog redirects the writer logger.LogError uses and returns whatever
// was written during fn. It also resets writeDeadlineUnsupportedOnce so a test can
// observe the first-alarm behaviour regardless of which tests ran before it.
func captureErrorLog(t *testing.T, fn func()) string {
	t.Helper()

	common.LogWriterMu.Lock()
	previous := gin.DefaultErrorWriter
	buffer := &bytes.Buffer{}
	gin.DefaultErrorWriter = buffer
	common.LogWriterMu.Unlock()

	writeDeadlineUnsupportedOnce = sync.Once{}
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previous
		common.LogWriterMu.Unlock()
		writeDeadlineUnsupportedOnce = sync.Once{}
	})

	fn()

	common.LogWriterMu.Lock()
	defer common.LogWriterMu.Unlock()
	return buffer.String()
}

// failingDeadlineWriter reports a deadline failure that is NOT ErrNotSupported —
// a transient per-connection error rather than a writer chain that cannot carry a
// deadline at all.
type failingDeadlineWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *failingDeadlineWriter) SetWriteDeadline(time.Time) error { return w.err }

// Losing the write deadline is what makes the unconditional wg.Wait() in cleanup
// able to hang forever, and http.NewResponseController resolves it by walking the
// Unwrap() chain — so a single wrapper added without Unwrap silently removes the
// bound. The alarm must therefore actually reach the log, and it must survive being
// called once per streamed chunk without flooding it.
func TestExtendWriteDeadlineAlarmsOnceWhenUnsupported(t *testing.T) {
	// httptest.ResponseRecorder has no SetWriteDeadline, so the controller reports
	// ErrNotSupported exactly as a production wrapper missing Unwrap would.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	logged := captureErrorLog(t, func() {
		for range 50 {
			ExtendWriteDeadline(c)
		}
	})

	assert.Equal(t, 1, strings.Count(logged, "without a write deadline"),
		"a writer chain that cannot carry a deadline is a static property, so it must alarm once per process, not once per chunk")
	assert.Contains(t, logged, "SetWriteDeadline",
		"the alarm has to name the missing capability to be actionable")
}

// A writer that does support deadlines must stay silent: this is the production
// path (pingAwareWriter -> gin responseWriter -> net/http response), and an alarm
// here would train the operator to ignore the real one.
func TestExtendWriteDeadlineSilentWhenSupported(t *testing.T) {
	w := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	logged := captureErrorLog(t, func() {
		ExtendWriteDeadline(c)
	})

	assert.Empty(t, logged, "arming a deadline successfully must not log")

	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.deadlines, 1, "the deadline must actually be armed")
	assert.False(t, w.deadlines[0].IsZero(), "the armed deadline must be a future instant, not the zero clear")
}

// A transient deadline error is per-call, not a property of the code, so it must
// not be collapsed by the ErrNotSupported once — otherwise a connection failing
// every write would report a single line and then go quiet.
func TestExtendWriteDeadlineReportsTransientErrorsEveryTime(t *testing.T) {
	w := &failingDeadlineWriter{
		ResponseRecorder: httptest.NewRecorder(),
		err:              errors.New("connection reset by peer"),
	}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	logged := captureErrorLog(t, func() {
		ExtendWriteDeadline(c)
		ExtendWriteDeadline(c)
		ExtendWriteDeadline(c)
	})

	assert.Equal(t, 3, strings.Count(logged, "failed to extend stream write deadline"),
		"a transient failure recurs per write and must be reported per write")
	assert.NotContains(t, logged, "without a write deadline",
		"a transient failure is not the unsupported-writer alarm and must not be reported as one")
}
