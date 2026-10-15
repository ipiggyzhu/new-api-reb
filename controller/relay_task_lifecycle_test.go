package controller

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type taskLifecycleTransport struct {
	base         http.RoundTripper
	responseBody *taskLifecycleBody
	panicRead    bool
}

func (tr *taskLifecycleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := tr.base.RoundTrip(req)
	if err == nil {
		tr.responseBody = &taskLifecycleBody{ReadCloser: resp.Body, panicRead: tr.panicRead}
		resp.Body = tr.responseBody
	}
	return resp, err
}

type taskLifecycleBody struct {
	io.ReadCloser
	read      int
	closed    bool
	panicRead bool
}

func (body *taskLifecycleBody) Read(p []byte) (int, error) {
	if body.panicRead {
		panic("upstream response parser panic")
	}
	n, err := body.ReadCloser.Read(p)
	body.read += n
	return n, err
}

func (body *taskLifecycleBody) Close() error {
	body.closed = true
	return body.ReadCloser.Close()
}

func TestRelayTaskErrorResponseIsBoundedAndClosed(t *testing.T) {
	router, _ := setupRelayTaskRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, strings.Repeat("x", 2<<20))
	}), 0, 0)
	client := service.GetHttpClient()
	transport := &taskLifecycleTransport{base: client.Transport}
	client.Transport = transport
	t.Cleanup(func() { client.Transport = transport.base })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, taskRoutingRequest("{\"prompt\":\"hello\"}"))
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotNil(t, transport.responseBody)
	assert.LessOrEqual(t, transport.responseBody.read, 1<<20, "upstream error bodies must not be buffered without a bound")
	assert.True(t, transport.responseBody.closed, "error responses must release their upstream connection")
}

func TestRelayTaskInsertFailureRefundsInsteadOfReturningLostTask(t *testing.T) {
	var preConsumedUser, preConsumedToken int
	var db *gorm.DB
	router, db := setupRelayTaskRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var user model.User
		var token model.Token
		if err := db.First(&user, 991).Error; err != nil {
			t.Error(err)
		}
		if err := db.First(&token, 992).Error; err != nil {
			t.Error(err)
		}
		preConsumedUser, preConsumedToken = user.Quota, token.RemainQuota
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"code\":\"success\",\"message\":\"\",\"data\":\"accepted-upstream-task\"}")
	}), 0.01, 3)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 991).Update("quota", 100_000).Error)
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("fail_task_insert", func(tx *gorm.DB) {
		if tx.Statement.Table == "tasks" {
			tx.AddError(errors.New("task persistence unavailable"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove("fail_task_insert")) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, taskRoutingRequest("{\"prompt\":\"hello\"}"))
	assert.Equal(t, 95_000, preConsumedUser, "fixture must exercise real wallet pre-consumption")
	assert.Equal(t, 95_000, preConsumedToken, "fixture must exercise real token pre-consumption")
	assert.Equal(t, http.StatusInternalServerError, recorder.Code, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "\"data\":\"task_", "a task that cannot be looked up must not be returned as success")
	require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 2*time.Second, time.Millisecond)
	var user model.User
	var token model.Token
	require.NoError(t, db.First(&user, 991).Error)
	require.NoError(t, db.First(&token, 992).Error)
	assert.Equal(t, 100_000, user.Quota)
	assert.Equal(t, 100_000, token.RemainQuota)
	assert.Zero(t, user.UsedQuota)
	var count int64
	require.NoError(t, db.Model(&model.Task{}).Count(&count).Error)
	assert.Zero(t, count)
}
