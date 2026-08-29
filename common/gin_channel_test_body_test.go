package common

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetBodyStorageWithNilRequestBody documents what a channel test sees.
// controller/channel-test.go builds its synthetic request with a nil body
// (httptest.NewRequestWithContext(..., nil)) and passes the payload as a Go
// struct instead, so any code path that reads the gin body during a channel
// test gets an empty storage rather than the request that is about to be sent.
func TestGetBodyStorageWithNilRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	storage, err := GetBodyStorage(ctx)
	require.NoError(t, err, "a nil body must not fail; it yields empty storage")
	require.NotNil(t, storage)

	data, err := storage.Bytes()
	require.NoError(t, err)
	assert.Empty(t, data, "channel test body storage is empty, not the synthetic request")

	_, err = storage.Seek(0, io.SeekStart)
	require.NoError(t, err)
	read, err := io.ReadAll(ReaderOnly(storage))
	require.NoError(t, err)
	assert.Empty(t, read, "passthrough would forward an empty body upstream")
}
