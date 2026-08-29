package controller

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stripePricingFixture(t *testing.T) {
	t.Helper()
	originalUnitPrice := setting.StripeUnitPrice
	originalMinTopUp := setting.StripeMinTopUp
	originalQuotaDisplayType := operation_setting.GetGeneralSetting().QuotaDisplayType
	originalDiscounts := operation_setting.GetPaymentSetting().AmountDiscount
	originalTopupGroupRatio := common.TopupGroupRatio2JSONString()

	t.Cleanup(func() {
		setting.StripeUnitPrice = originalUnitPrice
		setting.StripeMinTopUp = originalMinTopUp
		operation_setting.GetGeneralSetting().QuotaDisplayType = originalQuotaDisplayType
		operation_setting.GetPaymentSetting().AmountDiscount = originalDiscounts
		require.NoError(t, common.UpdateTopupGroupRatioByJSONString(originalTopupGroupRatio))
	})
}

func TestGetStripeTopupPlan_ChargeAndCreditShareOneCalculation(t *testing.T) {
	stripePricingFixture(t)

	setting.StripeUnitPrice = 8.0
	operation_setting.GetPaymentSetting().AmountDiscount = map[int]float64{90: 0.9}
	require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"default":1,"vip":1.5,"discounted":0.8,"neg":-1}`))

	tokens := int64(common.QuotaPerUnit)

	testCases := []struct {
		name             string
		quotaDisplayType string
		amount           int64
		group            string
		wantQuantity     int64
		wantPayMoney     float64
		wantCreditUnits  float64
		wantCreditAmount int64
		wantErr          bool
	}{
		{
			name:             "usd base ratio 1",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           100,
			group:            "default",
			wantQuantity:     100,
			wantPayMoney:     800,
			wantCreditUnits:  100,
			wantCreditAmount: 100,
		},
		{
			name:             "usd ratio 1.5 scales charge not credit",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           100,
			group:            "vip",
			wantQuantity:     150,
			wantPayMoney:     1200,
			wantCreditUnits:  100,
			wantCreditAmount: 100,
		},
		{
			name:             "usd discount group ratio 0.8 lowers charge not credit",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           100,
			group:            "discounted",
			wantQuantity:     80,
			wantPayMoney:     640,
			wantCreditUnits:  100,
			wantCreditAmount: 100,
		},
		{
			name:             "usd preset amount discount lowers charge not credit",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           90,
			group:            "default",
			wantQuantity:     81,
			wantPayMoney:     648,
			wantCreditUnits:  90,
			wantCreditAmount: 90,
		},
		{
			name:             "usd ratio and discount combine with credit derived from rounded quantity",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           90,
			group:            "vip",
			wantQuantity:     122,
			wantPayMoney:     976,
			wantCreditUnits:  90.37037037037037,
			wantCreditAmount: 90,
		},
		{
			name:             "usd fractional scaled quantity rounds half away from zero",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           101,
			group:            "vip",
			wantQuantity:     152,
			wantPayMoney:     1216,
			wantCreditUnits:  101.33333333333333,
			wantCreditAmount: 101,
		},
		{
			name:             "tokens display converts one quota unit",
			quotaDisplayType: operation_setting.QuotaDisplayTypeTokens,
			amount:           tokens,
			group:            "default",
			wantQuantity:     1,
			wantPayMoney:     8,
			wantCreditUnits:  1,
			wantCreditAmount: 1,
		},
		{
			name:             "tokens display with ratio 1.5 scales charge not credit",
			quotaDisplayType: operation_setting.QuotaDisplayTypeTokens,
			amount:           2 * tokens,
			group:            "vip",
			wantQuantity:     3,
			wantPayMoney:     24,
			wantCreditUnits:  2,
			wantCreditAmount: 2,
		},
		{
			name:             "tokens below half a unit rejected",
			quotaDisplayType: operation_setting.QuotaDisplayTypeTokens,
			amount:           tokens / 5,
			group:            "default",
			wantErr:          true,
		},
		{
			name:             "zero amount rejected",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           0,
			group:            "default",
			wantErr:          true,
		},
		{
			name:             "negative ratio can never produce a charge",
			quotaDisplayType: operation_setting.QuotaDisplayTypeUSD,
			amount:           100,
			group:            "neg",
			wantErr:          true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			operation_setting.GetGeneralSetting().QuotaDisplayType = tc.quotaDisplayType
			plan, err := getStripeTopupPlan(tc.amount, tc.group)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuantity, plan.quantity)
			assert.InDelta(t, tc.wantPayMoney, plan.payMoney, 1e-9)
			assert.InDelta(t, tc.wantCreditUnits, plan.creditUnits, 1e-9)
			assert.Equal(t, tc.wantCreditAmount, plan.creditAmount)
			// The charge is always the quoted unit price times the exact
			// integer quantity Stripe will bill.
			assert.InDelta(t, float64(plan.quantity)*setting.StripeUnitPrice, plan.payMoney, 1e-9)
		})
	}
}

func TestStripeTopupBoundsFollowQuotaDisplayType(t *testing.T) {
	stripePricingFixture(t)
	setting.StripeMinTopUp = 5

	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD
	assert.Equal(t, int64(5), getStripeMinTopup())
	assert.Equal(t, int64(10000), getStripeMaxTopup())

	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeTokens
	assert.Equal(t, int64(5*common.QuotaPerUnit), getStripeMinTopup())
	assert.Equal(t, int64(10000*common.QuotaPerUnit), getStripeMaxTopup())
	// Regression: with the unscaled 10000 cap, every TOKENS-mode request was
	// rejected because the minimum exceeded the maximum.
	assert.LessOrEqual(t, getStripeMinTopup(), getStripeMaxTopup())
}

func TestRequestStripePay_TokensDisplayScalesAmountBounds(t *testing.T) {
	stripePricingFixture(t)
	gin.SetMode(gin.TestMode)

	setting.StripeMinTopUp = 1
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeTokens

	maxTokens := int64(10000 * common.QuotaPerUnit)

	testCases := []struct {
		name        string
		amount      int64
		wantMessage string
	}{
		{
			name:        "above scaled cap rejected with scaled bound",
			amount:      maxTokens + 1,
			wantMessage: "充值数量不能大于 5000000000",
		},
		{
			name:        "below scaled minimum rejected with scaled bound",
			amount:      int64(common.QuotaPerUnit) - 1,
			wantMessage: "充值数量不能小于 500000",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/api/user/stripe/pay", nil)

			stripeAdaptor.RequestPay(c, &StripePayRequest{
				Amount:        tc.amount,
				PaymentMethod: model.PaymentMethodStripe,
			})

			var resp struct {
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantMessage, resp.Message)
		})
	}
}
