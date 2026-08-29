package service

import (
	"errors"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBillingSettler mirrors BillingSession's Reserve/Settle arithmetic while
// recording every funding movement, so the tests can assert a realtime session
// is charged its usage-derived total exactly once.
type fakeBillingSettler struct {
	preConsumed  int
	charged      int
	reserveCalls []int
	settleCalls  []int
	reserveErr   error
	settled      bool
}

var _ relaycommon.BillingSettler = (*fakeBillingSettler)(nil)

func (f *fakeBillingSettler) Settle(actualQuota int) error {
	f.settleCalls = append(f.settleCalls, actualQuota)
	if f.settled {
		return nil
	}
	f.charged += actualQuota - f.preConsumed
	f.settled = true
	return nil
}

func (f *fakeBillingSettler) Refund(*gin.Context) {}

func (f *fakeBillingSettler) NeedsRefund() bool { return false }

func (f *fakeBillingSettler) GetPreConsumedQuota() int { return f.preConsumed }

func (f *fakeBillingSettler) Reserve(targetQuota int) error {
	f.reserveCalls = append(f.reserveCalls, targetQuota)
	if f.reserveErr != nil {
		return f.reserveErr
	}
	if f.settled || targetQuota <= f.preConsumed {
		return nil
	}
	delta := targetQuota - f.preConsumed
	f.charged += delta
	f.preConsumed += delta
	return nil
}

func newRealtimeTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/v1/realtime", nil)
	return ctx
}

func newRealtimeRelayInfo(priceData types.PriceData, settler *fakeBillingSettler) *relaycommon.RelayInfo {
	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAIRealtime,
		OriginModelName: "gpt-4o-realtime-quota-test",
		UserQuota:       1_000_000_000,
		StartTime:       time.Now(),
		PriceData:       priceData,
	}
	if settler != nil {
		info.Billing = settler
		info.FinalPreConsumedQuota = settler.preConsumed
	}
	return info
}

func textInputUsage(tokens int) *dto.RealtimeUsage {
	return &dto.RealtimeUsage{
		TotalTokens: tokens,
		InputTokens: tokens,
		InputTokenDetails: dto.InputTokenDetails{
			TextTokens: tokens,
		},
	}
}

// A realtime session incrementally charges each response.done segment and then
// settles the full-session usage. The increments must be reservations that the
// final settle trues up against, so the wallet moves by exactly the session
// total — not pre-consume + increments + full settle (~2x).
func TestPreWssConsumeQuotaReservesAndSettlesSessionTotalOnce(t *testing.T) {
	ctx := newRealtimeTestContext(t)

	settler := &fakeBillingSettler{preConsumed: 1000, charged: 1000}
	info := newRealtimeRelayInfo(types.PriceData{
		ModelRatio:     2,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}, settler)

	segments := []int{5000, 3000, 2000}
	totalTokens := 0
	for _, tokens := range segments {
		require.NoError(t, PreWssConsumeQuota(ctx, info, textInputUsage(tokens)))
		totalTokens += tokens
	}

	// Each segment reserves cumulatively: quota = tokens * modelRatio * groupRatio.
	assert.Equal(t, []int{11000, 17000, 21000}, settler.reserveCalls)
	assert.Equal(t, 21000, settler.charged)

	// Final settle uses the same calculation on the summed usage, exactly as
	// PostWssConsumeQuota does.
	totalQuota, clamp := calculateAudioQuota(QuotaInfo{
		InputDetails: TokenDetails{TextTokens: totalTokens},
		ModelName:    info.OriginModelName,
		ModelRatio:   info.PriceData.ModelRatio,
		GroupRatio:   info.PriceData.GroupRatioInfo.GroupRatio,
	})
	require.Nil(t, clamp)
	require.Equal(t, 20000, totalQuota)

	require.NoError(t, SettleBilling(ctx, info, totalQuota))
	assert.Equal(t, []int{totalQuota}, settler.settleCalls)
	assert.Equal(t, totalQuota, settler.charged)
}

// Per-call-priced realtime models must skip token-based increments entirely:
// the fixed price is pre-consumed up front and settled once. The guard has to
// read PriceData.UsePrice — RelayInfo.UsePrice is never assigned.
func TestPreWssConsumeQuotaSkipsPerCallPricedModels(t *testing.T) {
	ctx := newRealtimeTestContext(t)

	perCallQuota, clamp := calculateAudioQuota(QuotaInfo{
		UsePrice:   true,
		ModelPrice: 0.5,
		GroupRatio: 1,
	})
	require.Nil(t, clamp)
	require.Equal(t, int(0.5*common.QuotaPerUnit), perCallQuota)

	settler := &fakeBillingSettler{preConsumed: perCallQuota, charged: perCallQuota}
	info := newRealtimeRelayInfo(types.PriceData{
		UsePrice:       true,
		ModelPrice:     0.5,
		ModelRatio:     37.5,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}, settler)

	require.NoError(t, PreWssConsumeQuota(ctx, info, textInputUsage(20000)))
	require.NoError(t, PreWssConsumeQuota(ctx, info, textInputUsage(20000)))
	assert.Empty(t, settler.reserveCalls)

	require.NoError(t, SettleBilling(ctx, info, perCallQuota))
	assert.Equal(t, perCallQuota, settler.charged)
}

func TestPreWssConsumeQuotaFreeModelWithoutBillingSession(t *testing.T) {
	ctx := newRealtimeTestContext(t)
	info := newRealtimeRelayInfo(types.PriceData{
		FreeModel:      true,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 0},
	}, nil)

	require.NoError(t, PreWssConsumeQuota(ctx, info, textInputUsage(5000)))
}

func TestPreWssConsumeQuotaPropagatesReserveFailure(t *testing.T) {
	ctx := newRealtimeTestContext(t)

	reserveErr := errors.New("user quota is not enough")
	settler := &fakeBillingSettler{reserveErr: reserveErr}
	info := newRealtimeRelayInfo(types.PriceData{
		ModelRatio:     1,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}, settler)

	err := PreWssConsumeQuota(ctx, info, textInputUsage(100))
	require.ErrorIs(t, err, reserveErr)
	assert.Equal(t, 0, settler.charged)
}

func TestPreWssConsumeQuotaFailsLoudlyOnSaturation(t *testing.T) {
	ctx := newRealtimeTestContext(t)

	settler := &fakeBillingSettler{}
	info := newRealtimeRelayInfo(types.PriceData{
		ModelRatio:     2,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}, settler)

	err := PreWssConsumeQuota(ctx, info, textInputUsage(math.MaxInt32))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saturated")
	assert.Empty(t, settler.reserveCalls)
	assert.Equal(t, 0, settler.charged)
	require.NotNil(t, info.QuotaClamp)
}
