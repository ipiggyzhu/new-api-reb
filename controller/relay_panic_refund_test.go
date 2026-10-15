package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/channel_score"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Exercise the real controller, upstream response parser, wallet and token
// reservation, rather than calling the refund helper with a fake settler.
func TestRelayPanicRefundsPreConsumedQuota(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "task"}[task], func(t *testing.T) {
			var db *gorm.DB
			var preConsumedUser, preConsumedToken atomic.Int64
			var calls atomic.Int32
			router, db := setupCrossGroupBillingTest(t, task, 100_000, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var user model.User
				var token model.Token
				if err := db.First(&user, 991).Error; err != nil {
					t.Error(err)
				}
				if err := db.First(&token, 992).Error; err != nil {
					t.Error(err)
				}
				preConsumedUser.Store(int64(user.Quota))
				preConsumedToken.Store(int64(token.RemainQuota))
				w.Header().Set("Content-Type", "application/json")
				if task {
					_, _ = io.WriteString(w, `{"code":"success","data":"upstream-task"}`)
				} else {
					_, _ = io.WriteString(w, `{"id":"chatcmpl-local","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
				}
			}))
			client := service.GetHttpClient()
			transport := &taskLifecycleTransport{base: client.Transport, panicRead: true}
			client.Transport = transport
			t.Cleanup(func() { client.Transport = transport.base })
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, crossGroupRequest(task))

			assert.Equal(t, int64(95_000), preConsumedUser.Load(), "wallet was reserved before the upstream parser panicked")
			assert.Equal(t, int64(95_000), preConsumedToken.Load(), "token quota was reserved before the upstream parser panicked")
			assert.Equal(t, http.StatusInternalServerError, recorder.Code)
			assert.Equal(t, int32(1), calls.Load(), "a local parser panic must not be retried")
			require.NotNil(t, transport.responseBody)
			assert.True(t, transport.responseBody.closed)
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				var user model.User
				var token model.Token
				require.NoError(collect, db.First(&user, 991).Error)
				require.NoError(collect, db.First(&token, 992).Error)
				assert.Equal(collect, 100_000, user.Quota)
				assert.Equal(collect, 100_000, token.RemainQuota)
				assert.Zero(collect, user.UsedQuota)
			}, 2*time.Second, time.Millisecond)
			var taskCount int64
			require.NoError(t, db.Model(&model.Task{}).Count(&taskCount).Error)
			assert.Zero(t, taskCount)
			assert.Empty(t, channel_score.Snapshot(channel_score.ScoreFilter{}).Rows)
		})
	}
}
