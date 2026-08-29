package relay

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Adaptor.DoResponse declares its usage return as `any`, so an adaptor that
// returns a nil *dto.Usage hands back a non-nil interface holding a typed nil.
// Every billing path used to write usage.(*dto.Usage) and dereference the result:
// the one-value assertion succeeds on a typed nil, so the field access panicked.
// That panic unwound past controller.Relay's deferred refund, which only refunds
// when its error variable is non-nil — during an unwind it is nil, so the
// pre-consumed quota was neither refunded nor settled and the user was charged
// the estimate with no consume log. Failing the relay with a 500 keeps that
// refund on the normal path.
//
// The typed-nil row is the whole point: `usage == nil` is false for it, so a
// plain nil check would hand the same unusable pointer back to the caller.
func TestAdaptorUsageRejectsUnusableUsage(t *testing.T) {
	upstreamUsage := &dto.Usage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33}

	tests := []struct {
		name  string
		usage any
		want  *dto.Usage
	}{
		{name: "typed nil pointer", usage: (*dto.Usage)(nil)},
		{name: "untyped nil", usage: nil},
		{name: "wrong concrete type", usage: &dto.RealtimeUsage{TotalTokens: 7}},
		{name: "usable usage", usage: upstreamUsage, want: upstreamUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usageDto, apiErr := adaptorUsage(tt.usage)

			if tt.want != nil {
				require.Nil(t, apiErr, "a usable usage must not fail the relay")
				assert.Same(t, tt.want, usageDto, "a usable usage must round-trip as the same pointer the adaptor returned")
				return
			}

			require.NotNil(t, apiErr, "an unusable usage must fail the relay instead of reaching a dereference, so the deferred refund still runs on the normal error path")
			assert.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
			assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
			assert.Nil(t, usageDto, "a failed narrowing must not also hand back a pointer for the caller to dereference")
		})
	}
}
