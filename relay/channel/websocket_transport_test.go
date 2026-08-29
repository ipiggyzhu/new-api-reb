package channel

import (
	"net/http"
	"testing"

	appconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
)

func TestBuildResponsesWebsocketURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "https becomes wss", in: "https://chatgpt.com/backend-api/codex/responses", want: "wss://chatgpt.com/backend-api/codex/responses"},
		{name: "http becomes ws", in: "http://localhost:3000/v1/responses", want: "ws://localhost:3000/v1/responses"},
		{name: "query preserved", in: "https://x.openai.azure.com/openai/v1/responses?api-version=preview", want: "wss://x.openai.azure.com/openai/v1/responses?api-version=preview"},
		{name: "already wss rejected", in: "wss://api.openai.com/v1/responses", wantErr: true},
		{name: "no host rejected", in: "https:///v1/responses", wantErr: true},
		{name: "empty rejected", in: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildResponsesWebsocketURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsDefinitiveUpgradeRefusal pins the narrow list. The flag it drives survives
// until an admin saves the channel, so a status added here turns a transient
// upstream condition into a permanent silent downgrade.
func TestIsDefinitiveUpgradeRefusal(t *testing.T) {
	definitive := []int{
		http.StatusUpgradeRequired,
		http.StatusNotFound,
		http.StatusNotImplemented,
		http.StatusMethodNotAllowed,
	}
	for _, status := range definitive {
		if !isDefinitiveUpgradeRefusal(status) {
			t.Errorf("status %d should be a definitive refusal", status)
		}
	}

	// Each of these means something other than "this endpoint has no WebSocket
	// form", so none may persist a downgrade.
	ambiguous := []int{
		0, // transport error or timeout: no HTTP reply at all
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusBadRequest,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusOK,
	}
	for _, status := range ambiguous {
		if isDefinitiveUpgradeRefusal(status) {
			t.Errorf("status %d must not persist a downgrade", status)
		}
	}
}

func TestWebsocketFrameErrorStatus(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantStatus int
		wantIsErr  bool
	}{
		{name: "status field", payload: `{"type":"error","status":429}`, wantStatus: 429, wantIsErr: true},
		{name: "status_code fallback", payload: `{"type":"error","status_code":503}`, wantStatus: 503, wantIsErr: true},
		{name: "error without status defaults to 500", payload: `{"type":"error","error":{"message":"boom"}}`, wantStatus: http.StatusInternalServerError, wantIsErr: true},
		{name: "normal event is not an error", payload: `{"type":"response.output_text.delta","delta":"hi"}`},
		{name: "completion is not an error", payload: `{"type":"response.completed","response":{"id":"resp_1"}}`},
		// "error" must be the frame's own type; a field merely named error elsewhere
		// is part of a normal event and must not abort the stream.
		{name: "nested error field is not an error frame", payload: `{"type":"response.completed","response":{"error":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, isErr := websocketFrameErrorStatus([]byte(tc.payload))
			if isErr != tc.wantIsErr {
				t.Fatalf("isErr = %v, want %v", isErr, tc.wantIsErr)
			}
			if isErr && status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

func TestNormalizeWebsocketCompletion(t *testing.T) {
	// response.done is WebSocket-only; the SSE handlers and usage parsing match on
	// response.completed, so leaving it would drop usage for the whole turn.
	got := normalizeWebsocketCompletion([]byte(`{"type":"response.done","response":{"id":"resp_1"}}`))
	if status, _ := websocketFrameErrorStatus(got); status != 0 {
		t.Fatalf("normalization must not turn a completion into an error")
	}
	if want := `{"type":"response.completed","response":{"id":"resp_1"}}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}

	// Anything else passes through byte-identical.
	for _, payload := range []string{
		`{"type":"response.completed","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
	} {
		if got := string(normalizeWebsocketCompletion([]byte(payload))); got != payload {
			t.Fatalf("passthrough changed payload: got %s, want %s", got, payload)
		}
	}
}

// TestIsWebsocketTerminalEvent covers the stall case: failed and incomplete end the
// turn with no further frames, so treating either as non-terminal would leave the
// reader blocked until the idle deadline.
func TestIsWebsocketTerminalEvent(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.failed", "response.incomplete"} {
		if !isWebsocketTerminalEvent(eventType) {
			t.Errorf("%q should be terminal", eventType)
		}
	}
	for _, eventType := range []string{
		"response.created",
		"response.in_progress",
		"response.output_text.delta",
		"response.output_item.done",
		"",
	} {
		if isWebsocketTerminalEvent(eventType) {
			t.Errorf("%q should not be terminal", eventType)
		}
	}
}

func TestEncodeWebsocketFrameAsSSE(t *testing.T) {
	got := string(encodeWebsocketFrameAsSSE([]byte(`{"type":"response.created"}`)))
	// The blank line terminates the event; without it the scanner would merge
	// consecutive frames into one unparsable payload.
	if want := "data: {\"type\":\"response.created\"}\n\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestApplyResponsesWebsocketBetaHeader(t *testing.T) {
	// Absent: the opt-in is added, or upstream refuses the upgrade.
	header := make(http.Header)
	applyResponsesWebsocketBetaHeader(header)
	if got := header.Get("OpenAI-Beta"); got != websocketResponsesBetaHeader {
		t.Fatalf("got %q, want %q", got, websocketResponsesBetaHeader)
	}

	// Already opted in: the caller's pinned date must survive, since overwriting it
	// would silently move them to a protocol revision they did not ask for.
	pinned := "responses_websockets=2025-01-01"
	header = make(http.Header)
	header.Set("OpenAI-Beta", pinned)
	applyResponsesWebsocketBetaHeader(header)
	if got := header.Get("OpenAI-Beta"); got != pinned {
		t.Fatalf("pinned value overwritten: got %q, want %q", got, pinned)
	}

	// An unrelated beta value is not an opt-in, so it is replaced.
	header = make(http.Header)
	header.Set("OpenAI-Beta", "assistants=v2")
	applyResponsesWebsocketBetaHeader(header)
	if got := header.Get("OpenAI-Beta"); got != websocketResponsesBetaHeader {
		t.Fatalf("got %q, want %q", got, websocketResponsesBetaHeader)
	}
}

func TestStripHTTPOnlyHandshakeHeaders(t *testing.T) {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	header.Set("Content-Length", "123")
	header.Set("Authorization", "Bearer sk-test")
	header.Set("chatgpt-account-id", "acct_1")

	stripHTTPOnlyHandshakeHeaders(header)

	// The handshake is a bodyless GET; some upstreams reject an upgrade that carries
	// a Content-Type, and Accept would advertise a transport this request is not using.
	for _, name := range []string{"Content-Type", "Accept", "Content-Length"} {
		if got := header.Get(name); got != "" {
			t.Errorf("%s should have been stripped, got %q", name, got)
		}
	}
	// Credentials must survive: they are what authenticates the handshake.
	if got := header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization was dropped: %q", got)
	}
	if got := header.Get("chatgpt-account-id"); got != "acct_1" {
		t.Errorf("chatgpt-account-id was dropped: %q", got)
	}
}

func newWebsocketGateInfo() *relaycommon.RelayInfo {
	// ChannelMeta is an embedded pointer, so the promoted fields cannot be set in
	// the RelayInfo literal and must go through it explicitly.
	return &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiType:              appconstant.APITypeCodex,
			ChannelSetting:       dto.ChannelSettings{WebsocketTransport: true},
			ChannelOtherSettings: dto.ChannelOtherSettings{},
		},
	}
}

func TestShouldTryResponsesWebsocket(t *testing.T) {
	if !ShouldTryResponsesWebsocket(newWebsocketGateInfo()) {
		t.Fatal("a codex responses channel with the switch on should attempt websocket")
	}

	if info := newWebsocketGateInfo(); func() bool {
		info.ApiType = appconstant.APITypeOpenAI
		return !ShouldTryResponsesWebsocket(info)
	}() {
		t.Error("openai responses channels should attempt websocket too")
	}

	t.Run("nil info", func(t *testing.T) {
		if ShouldTryResponsesWebsocket(nil) {
			t.Error("nil info must not attempt websocket")
		}
	})

	// ChannelMeta is an embedded pointer and the settings fields are promoted
	// through it, so an uninitialized RelayInfo must be refused rather than
	// panicking on the request path.
	t.Run("nil channel meta", func(t *testing.T) {
		info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeResponses}
		if ShouldTryResponsesWebsocket(info) {
			t.Error("nil ChannelMeta must not attempt websocket")
		}
	})

	t.Run("switch off", func(t *testing.T) {
		info := newWebsocketGateInfo()
		info.ChannelSetting.WebsocketTransport = false
		if ShouldTryResponsesWebsocket(info) {
			t.Error("websocket must be opt-in")
		}
	})

	// The detected-unsupported flag has to beat the admin's switch, otherwise a
	// channel behind an HTTP-only relay would pay a failed handshake on every request.
	t.Run("detected unsupported overrides the switch", func(t *testing.T) {
		info := newWebsocketGateInfo()
		info.ChannelOtherSettings.WebsocketUnsupported = true
		if ShouldTryResponsesWebsocket(info) {
			t.Error("a persisted downgrade must win over the switch")
		}
	})

	// /v1/responses/compact has no WebSocket form.
	t.Run("compact mode excluded", func(t *testing.T) {
		info := newWebsocketGateInfo()
		info.RelayMode = relayconstant.RelayModeResponsesCompact
		if ShouldTryResponsesWebsocket(info) {
			t.Error("compact must stay on HTTP")
		}
	})

	t.Run("other relay modes excluded", func(t *testing.T) {
		info := newWebsocketGateInfo()
		info.RelayMode = relayconstant.RelayModeRealtime
		if ShouldTryResponsesWebsocket(info) {
			t.Error("only /v1/responses has a websocket form")
		}
	})

	// A channel test reuses this relay code. Probing an upgrade there would report
	// "upgrade failed" for a working model, and could persist a downgrade from a
	// synthetic request.
	t.Run("channel test excluded", func(t *testing.T) {
		info := newWebsocketGateInfo()
		info.IsChannelTest = true
		if ShouldTryResponsesWebsocket(info) {
			t.Error("channel tests must use the HTTP path")
		}
	})

	t.Run("unsupported api type excluded", func(t *testing.T) {
		info := newWebsocketGateInfo()
		info.ApiType = -1
		if ShouldTryResponsesWebsocket(info) {
			t.Error("only openai and codex api types may attempt websocket")
		}
	})
}
