package privacy

import (
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrivacyProtectConversationAndNestedToolArguments(t *testing.T) {
	// All values are synthetic fixtures, never working credentials.
	password := "fixture-中文-\"quote\\line\nend"
	arguments, err := common.Marshal(map[string]any{"password": password, "port": 0})
	require.NoError(t, err)
	body, err := common.Marshal(map[string]any{
		"model": "user", "temperature": 0, "stream": false,
		"messages": []any{
			map[string]any{"role": "tool", "content": password + " 203.0.113.7"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "connect", "arguments": string(arguments)}}}},
		},
		"future": map[string]any{"counter": uint64(18446744073709551615)},
	})
	require.NoError(t, err)
	session := NewSession()
	protected, err := session.ProtectJSON(body, Options{ProtectIPs: true})
	require.NoError(t, err)
	assert.NotContains(t, string(protected), "fixture-")
	assert.NotContains(t, string(protected), "203.0.113.7")
	assert.Equal(t, "18446744073709551615", gjson.GetBytes(protected, "future.counter").Raw)
	assert.Equal(t, "0", gjson.GetBytes(protected, "temperature").Raw)
	assert.Equal(t, "false", gjson.GetBytes(protected, "stream").Raw)
	assert.Equal(t, "tool", gjson.GetBytes(protected, "messages.0.role").String())
	assert.Equal(t, "user", gjson.GetBytes(protected, "model").String())
	restored, err := session.RestoreJSON(protected, true)
	require.NoError(t, err)
	assert.Equal(t, password+" 203.0.113.7", gjson.GetBytes(restored, "messages.0.content").String())
	inner := gjson.GetBytes(restored, "messages.1.tool_calls.0.function.arguments").String()
	require.True(t, gjson.Valid(inner))
	assert.Equal(t, password, gjson.Get(inner, "password").String())
	assert.Equal(t, "0", gjson.Get(inner, "port").Raw)
	// Repeated protection (conversion, retry, HTTP fallback) is idempotent.
	again, err := session.ProtectJSON(protected, Options{ProtectIPs: true})
	require.NoError(t, err)
	assert.JSONEq(t, string(protected), string(again))
}

func TestPrivacyDetectionAndExplicitValues(t *testing.T) {
	tests := []struct{ name, input, secret string }{
		{"Chinese", "服务器 IP 203.0.113.8，密码是 fixture-Pass! 请检查磁盘", "fixture-Pass!"},
		{"environment", "DB_PASSWORD='fixture with spaces'", "fixture with spaces"},
		{"connection", "postgres://alice:fixture-pass@db.example.test/app", "fixture-pass"},
		{"bearer", "Authorization: Bearer fixture-token.123", "fixture-token.123"},
		{"token", "use sk-proj-abcdefghijklmnopqrstuvwxyz123456", "sk-proj-abcdefghijklmnopqrstuvwxyz123456"},
		{"private key", "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----"},
		{"IPv6", "connect [2001:db8::1]:22", "2001:db8::1"},
		{"Chinese plain word", "默认密码是 fixtureword", "fixtureword"},
		{"Chinese label then space", "登录密码 Zx9!fixture#mP 帮我试一下", "Zx9!fixture#mP"},
		{"Chinese label between words", "账号 admin 密码 Zx9!fixture#mP", "Zx9!fixture#mP"},
		{"Chinese label without space", "账号admin密码Fixture12345帮我登录", "Fixture12345"},
		{"English sentence", "my password is fixture-Hunter2. Please log in", "fixture-Hunter2"},
		{"English sentence with colon", "The API key is: fixture_Key99", "fixture_Key99"},
		{"passphrase", "passphrase: fixture-phrase", "fixture-phrase"},
		{"bare token", "token=fixture-tok-123", "fixture-tok-123"},
		{"verification code", "验证码是 482913", "482913"},
		{"mysql flag", "mysql -u root -pfixture-Pw9 -h db.example.test", "fixture-Pw9"},
		{"mysql quoted flag", "mysqldump -uroot -p'fixture Pw9' app", "fixture Pw9"},
		{"sshpass", "sshpass -p fixture-Pw9 ssh root@example.test", "fixture-Pw9"},
		{"redis", "redis-cli -h cache.example.test -a fixture-Pw9 ping", "fixture-Pw9"},
		{"curl user", "curl -u admin:fixture-Pw9 https://example.test/status", "fixture-Pw9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := NewSession()
			body, err := common.Marshal(map[string]string{"content": tc.input})
			require.NoError(t, err)
			protected, err := session.ProtectJSON(body, Options{ProtectIPs: true})
			require.NoError(t, err)
			content := gjson.GetBytes(protected, "content").String()
			assert.NotContains(t, content, tc.secret)
			assert.Equal(t, tc.input, session.Restore(content))
		})
	}
	session := NewSession()
	require.NoError(t, session.Add("abcd"))
	masked := session.Mask("abcd")
	require.NotEqual(t, "abcd", masked, "the first mask must actually protect the value")
	require.Regexp(t, "^"+Prefix+"[0-9a-f]{32}__$", masked)
	// Derive a valid value from an issued ID so the collision is guaranteed.
	collidingValue := masked[len(Prefix) : len(Prefix)+4]
	require.NoError(t, session.Add(collidingValue))
	maskedCollision := session.Mask(collidingValue)
	require.NotEqual(t, collidingValue, maskedCollision)
	assert.Equal(t, collidingValue, session.Restore(maskedCollision))
	assert.Equal(t, masked+" "+maskedCollision, session.Mask(masked+" "+collidingValue))
	assert.Equal(t, masked, session.Mask(masked), "short secrets cannot rewrite reference identifiers")
	assert.Equal(t, "abcd", session.Restore(masked))
	placeholderShapedPassword := "__NA_PRIV_0123456789abcdef0123456789abcdef__"
	require.NoError(t, session.Add(placeholderShapedPassword))
	maskedPassword := session.Mask(placeholderShapedPassword)
	assert.NotEqual(t, placeholderShapedPassword, maskedPassword)
	assert.Equal(t, placeholderShapedPassword, session.Restore(maskedPassword))
}

func TestPrivacySpokenCredentialsLeaveOrdinaryProseAlone(t *testing.T) {
	// Every stored value is replaced wherever it occurs, so prose that merely
	// follows a credential word must never be stored.
	for _, input := range []string{
		"password is required for every account",
		"the secret is out",
		"密码 是什么",
		"密码8位以上，口令 sudo 才能用",
		"mysql -P3306 -u root -p appdb",
		"mysql --protocol=tcp -e 'select 1'",
		"docker run -p8080:80 nginx",
	} {
		t.Run(input, func(t *testing.T) {
			session := NewSession()
			protected, err := session.ProtectText("content", input, Options{})
			require.NoError(t, err)
			assert.Equal(t, input, protected)
		})
	}
}

func TestPrivacySpokenCredentialIsHiddenWhereItReappears(t *testing.T) {
	// The sentence ends with a full stop; the tool output repeats the value bare.
	body := `{"messages":[{"role":"user","content":"my password is fixture-Hunter2."},{"role":"tool","content":"login fixture-Hunter2 ok"}],"input":{"dbPassword":"fixture-camel-9"},"echo":"fixture-camel-9"}`
	session := NewSession()
	protected, err := session.ProtectJSON([]byte(body), Options{})
	require.NoError(t, err)
	assert.NotContains(t, string(protected), "fixture-Hunter2")
	assert.NotContains(t, string(protected), "fixture-camel-9")
	restored, err := session.RestoreJSON(protected, true)
	require.NoError(t, err)
	assert.JSONEq(t, body, string(restored))
}

func TestPrivacyOpaqueContentPassesThroughUnchanged(t *testing.T) {
	// The four-letter secret also occurs inside every media payload below;
	// rewriting it there would corrupt the upload.
	body := `{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"密码是 AAAA"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},` +
		`{"type":"image","source":{"type":"base64","data":"AAAAAAAA"}},` +
		`{"type":"text","text":"data:image/png;base64,AAAA"}]}],` +
		`"input":[{"type":"reasoning","encrypted_content":"AAAAxx"}],` +
		`"tools":[{"type":"mcp","authorization":"AAAA"},{"type":"function","function":{"name":"run","description":"uses AAAA"}}],` +
		`"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}]}`
	session := NewSession()
	protected, err := session.ProtectJSON([]byte(body), Options{})
	require.NoError(t, err)
	assert.NotContains(t, gjson.GetBytes(protected, "messages.0.content.0.text").String(), "AAAA")
	assert.NotContains(t, gjson.GetBytes(protected, "tools.1.function.description").String(), "AAAA")
	for _, path := range []string{
		"messages.0.content.1", "messages.0.content.2", "messages.0.content.3.text",
		"input.0.encrypted_content", "tools.0", "contents.0.parts.0.inlineData",
	} {
		assert.JSONEq(t, gjson.Get(body, path).Raw, gjson.GetBytes(protected, path).Raw, path)
	}
}

func TestPrivacyOpaqueToolsByDefault(t *testing.T) {
	s := NewSession()
	require.NoError(t, s.Add("fixture-pass"))
	ref := s.Mask("fixture-pass")
	for _, body := range []string{
		fmt.Sprintf(`{"choices":[{"message":{"content":"%s","tool_calls":[{"function":{"arguments":"{\"password\":\"%s\"}"}}]}}]}`, ref, ref),
		fmt.Sprintf(`{"content":[{"type":"text","text":"%s"},{"type":"tool_use","input":{"password":"%s"}}]}`, ref, ref),
		fmt.Sprintf(`{"output":[{"type":"function_call","arguments":"{\"password\":\"%s\"}"}]}`, ref),
	} {
		out, err := s.RestoreJSON([]byte(body), false)
		require.NoError(t, err)
		assert.Contains(t, string(out), ref)
		out, err = s.RestoreJSON([]byte(body), true)
		require.NoError(t, err)
		assert.NotContains(t, string(out), ref)
		assert.Contains(t, string(out), "fixture-pass")
	}
}

func TestPrivacySessionEncryptionIsolationAndExpiry(t *testing.T) {
	store, err := NewStore(2, time.Minute)
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	session, id, err := store.Open("user:token", "")
	require.NoError(t, err)
	require.NoError(t, session.Add("fixture-secret"))
	require.NoError(t, store.Save("user:token", id, session))
	assert.NotContains(t, string(store.entries["user:token:"+id].data), "fixture-secret")
	_, _, err = store.Open("other:token", id)
	assert.ErrorIs(t, err, ErrSession)
	a, _, err := store.Open("user:token", id)
	require.NoError(t, err)
	b, _, err := store.Open("user:token", id)
	require.NoError(t, err)
	require.NoError(t, a.Add("parallel-a"))
	require.NoError(t, b.Add("parallel-b"))
	require.NoError(t, store.Save("user:token", id, a))
	require.NoError(t, store.Save("user:token", id, b))
	merged, _, err := store.Open("user:token", id)
	require.NoError(t, err)
	assert.Equal(t, "fixture-secret", merged.Restore(session.Mask("fixture-secret")))
	assert.Equal(t, "parallel-a", merged.Restore(a.Mask("parallel-a")))
	assert.Equal(t, "parallel-b", merged.Restore(b.Mask("parallel-b")))
	now = now.Add(time.Minute)
	_, _, err = store.Open("user:token", id)
	assert.ErrorIs(t, err, ErrSession)
}

func TestPrivacyUnsupportedContentAndDepth(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"https://example.test/secret.png"}}]}]}`,
		`{"tools":[{"type":"mcp","server_url":"https://example.test"}]}`,
		`{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}]}`,
	} {
		assert.Error(t, ValidateTextRequest([]byte(body)))
	}
	// Encrypted reasoning is issued by the provider from an already substituted
	// conversation, so replaying it is not an upload this layer has to refuse.
	require.NoError(t, ValidateTextRequest([]byte(`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`)))
	require.NoError(t, ValidateTextRequest([]byte(`{"tools":[{"type":"function","function":{"name":"ssh","parameters":{"type":"object","properties":{"password":{"type":"string"}}}}}]}`)))
	_, err := NewSession().ProtectJSON([]byte(strings.Repeat("[", 50)+`"hello"`+strings.Repeat("]", 50)), Options{})
	assert.ErrorIs(t, err, ErrLimit)
}

func TestPrivacyProtectQueryDecodesCredentialsBeforeReplacing(t *testing.T) {
	s := NewSession()
	got, err := s.ProtectQuery("host=203.0.113.22&password=fixture%2Dquery%20pass", Options{ProtectIPs: true})
	require.NoError(t, err)
	assert.NotContains(t, got, "fixture")
	assert.NotContains(t, got, "203.0.113.22")
	values, err := url.ParseQuery(got)
	require.NoError(t, err)
	assert.Equal(t, "fixture-query pass", s.Restore(values.Get("password")))
	assert.Equal(t, "203.0.113.22", s.Restore(values.Get("host")))
}

func TestPrivacyAddressOverflowNeverCostsACredential(t *testing.T) {
	// A pasted log names more addresses than one session holds, and the
	// password comes last.
	var log strings.Builder
	for i := 0; i < MaxSecrets+20; i++ {
		fmt.Fprintf(&log, "203.0.%d.%d refused\n", i/250, i%250+1)
	}
	body, err := common.Marshal(map[string]any{"messages": []any{
		map[string]string{"role": "tool", "content": log.String()},
		map[string]string{"role": "user", "content": "password=fixture-pass"},
	}})
	require.NoError(t, err)

	siteWide := NewSession()
	siteWide.BestEffort = true
	protected, err := siteWide.ProtectJSON(body, Options{ProtectIPs: true})
	require.NoError(t, err)
	assert.NotContains(t, string(protected), "fixture-pass")
	assert.NotContains(t, string(protected), "203.0.0.1 ")
	restored, err := siteWide.RestoreJSON(protected, true)
	require.NoError(t, err)
	assert.JSONEq(t, string(body), string(restored))

	_, err = NewSession().ProtectJSON(body, Options{ProtectIPs: true})
	assert.ErrorIs(t, err, ErrLimit, "a request that asked for protection is refused, not partly protected")
}

func TestPrivacyFullStoreMakesRoomForNewSessions(t *testing.T) {
	store, err := NewStore(2, time.Minute)
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	var ids []string
	for i := 0; i < 3; i++ {
		session, id, err := store.Open("user:token", "")
		require.NoError(t, err)
		require.NoError(t, session.Add(fmt.Sprintf("fixture-secret-%d", i)))
		require.NoError(t, store.Save("user:token", id, session))
		ids = append(ids, id)
		now = now.Add(time.Second)
	}
	_, _, err = store.Open("user:token", ids[0])
	assert.ErrorIs(t, err, ErrSession, "the session nearest to expiry gave way")
	for _, id := range ids[1:] {
		_, _, err = store.Open("user:token", id)
		assert.NoError(t, err)
	}
}

func TestPrivacyOutboundBodyUnderSiteWideSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	form := "--fixture\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nfixture-pass\r\n--fixture--\r\n"
	oversized := `{"input":"fixture-pass ` + strings.Repeat("A", OutboundLimit) + `"}`
	for _, tc := range []struct {
		name, body string
		bestEffort bool
		want       string
		wantErr    error
	}{
		{"converted form passes as built", form, true, form, nil},
		{"oversized body passes as built", oversized, true, oversized, nil},
		{"oversized body is refused on request", oversized, false, "", ErrLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			session := NewSession()
			session.BestEffort = tc.bestEffort
			require.NoError(t, session.Add("fixture-pass"))
			c.Set(ContextKey, session)
			reader, err := ProtectReader(c, strings.NewReader(tc.body))
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.True(t, tc.want == string(got), "body must reach the provider unchanged")
		})
	}
	// Readable JSON is still substituted, whatever else rides along.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	session := NewSession()
	session.BestEffort = true
	c.Set(ContextKey, session)
	assert.NoError(t, CheckOutboundJSON(c, []byte(form)), "the final check has nothing to read in a form")
	protected, err := ProtectOutboundJSON(c, []byte(`{"system":"password=fixture-pass","messages":[{"content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`))
	require.NoError(t, err)
	assert.NotContains(t, string(protected), "fixture-pass")
	assert.Equal(t, "https://example.test/a.png", gjson.GetBytes(protected, "messages.0.content.0.image_url.url").String())
}
