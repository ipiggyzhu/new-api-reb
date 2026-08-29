package model

// RefreshPricing 强制立即重新计算与定价相关的缓存。
// 该方法用于需要最新数据的内部管理 API，
// 因此会绕过默认的 1 分钟延迟刷新。
func RefreshPricing() {
	updatePricingLock.Lock()
	defer updatePricingLock.Unlock()

	// updatePricing 在发布阶段自行持有 modelSupportEndpointsLock，
	// 这里再加锁会与其自身死锁。
	updatePricing()
}
