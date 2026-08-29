package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// updatePricing 在发布阶段自行持有 modelSupportEndpointsLock，因此
// RefreshPricing 不得在外层重复加同一把非重入锁——否则管理端每次
// 模型元数据变更都会触发自死锁，进而卡死所有定价读取与渠道缓存失效。
// 本测试钉死"RefreshPricing 必须能返回"这一契约。
func TestRefreshPricingReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		RefreshPricing()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.Fail(t, "RefreshPricing did not return; lock ordering deadlock reintroduced")
	}
}
