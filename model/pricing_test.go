package model

import (
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupPricingCacheTest(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&Channel{}, &Ability{}, &Model{}, &Vendor{}))
	for _, table := range []string{"abilities", "channels", "models", "vendors"} {
		require.NoError(t, DB.Exec("DELETE FROM "+table).Error)
	}
	InvalidatePricingCache()
	t.Cleanup(func() {
		for _, table := range []string{"abilities", "channels", "models", "vendors"} {
			require.NoError(t, DB.Exec("DELETE FROM "+table).Error)
		}
		InvalidatePricingCache()
	})
}

// Regression test for the data race between the GetPricing fast path and
// updatePricing rebuilding the cache in place: run with -race, readers must
// never observe a partially built pricing list.
func TestGetPricingConcurrentWithCacheInvalidation(t *testing.T) {
	setupPricingCacheTest(t)

	require.NoError(t, DB.Create(&Channel{
		Id:     501,
		Type:   constant.ChannelTypeOpenAI,
		Key:    "key-501",
		Status: common.ChannelStatusEnabled,
		Name:   "channel-501",
	}).Error)
	modelNames := []string{"gpt-4o", "gpt-4o-mini", "claude-3-5-sonnet", "text-embedding-3-small"}
	for _, name := range modelNames {
		require.NoError(t, DB.Create(&Ability{
			Group:     "default",
			Model:     name,
			ChannelId: 501,
			Enabled:   true,
		}).Error)
	}

	var wg sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				pricings := GetPricing()
				// A published pricing list must always be complete: readers
				// overlapping a rebuild must never see a truncated snapshot.
				if len(pricings) != 0 {
					assert.Len(t, pricings, len(modelNames))
				}
				for _, pricing := range pricings {
					_ = pricing.ModelName
				}
				_ = GetVendors()
				_ = GetSupportedEndpointMap()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			InvalidatePricingCache()
		}
	}()
	wg.Wait()

	byModel := make(map[string]struct{})
	for _, pricing := range GetPricing() {
		byModel[pricing.ModelName] = struct{}{}
	}
	require.Len(t, byModel, len(modelNames))
	for _, name := range modelNames {
		assert.Contains(t, byModel, name)
	}
}
