package privacy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type compactCredentialCase struct {
	name  string
	input string
	want  []string
}

// Assertions deliberately report booleans and counts, never conversation text.
// This fixture checks the public contract through both discovery entry points.
func assertCompactCredentialCase(t *testing.T, tc compactCredentialCase, options Options) {
	t.Helper()
	for _, transport := range []string{"text", "json"} {
		t.Run(transport, func(t *testing.T) {
			session := NewSession()
			var protected string
			if transport == "text" {
				var err error
				protected, err = session.ProtectText("content", tc.input, options)
				require.NoError(t, err)
			} else {
				body, err := common.Marshal(map[string]any{
					"messages": []map[string]string{{"role": "user", "content": tc.input}},
				})
				require.NoError(t, err)
				out, err := session.ProtectJSON(body, options)
				require.NoError(t, err)
				protected = gjson.GetBytes(out, "messages.0.content").String()
				again, err := session.ProtectJSON(out, options)
				require.NoError(t, err)
				assert.True(t, string(out) == string(again), "repeated protection must preserve issued references")
			}
			stored := session.snapshot()
			assert.Equal(t, len(tc.want), len(stored), "exact registration count")
			refs := make(map[string]string, len(stored))
			for ref, plain := range stored {
				refs[plain] = ref
			}
			pairs := make([]string, 0, len(tc.want)*2)
			for i, plain := range tc.want {
				ref, ok := refs[plain]
				require.True(t, ok, "expected registration index %d", i)
				assert.True(t, referencePattern.MatchString(ref), "issued reference shape")
				assert.Equal(t, strings.Count(tc.input, plain), strings.Count(protected, ref), "repetitions share one reference at index %d", i)
				pairs = append(pairs, plain, ref)
			}
			want := strings.NewReplacer(pairs...).Replace(tc.input)
			assert.True(t, want == protected, "exact upstream text, including punctuation")
			assert.True(t, tc.input == session.Restore(protected), "lossless restoration")
			assert.True(t, protected == session.Mask(protected), "masking is idempotent")
		})
	}
}

func TestCompactCredentialMatrix(t *testing.T) {
	// Build every address, credential and complete trigger at runtime.
	ip := strings.Join([]string{"192", "0", "2", "28"}, ".")
	ip2 := strings.Join([]string{"198", "51", "100", "29"}, ".")
	invalid := strings.Join([]string{"999", "0", "2", "28"}, ".")
	ip6 := "2001" + ":" + "db8" + "::" + "28"
	short6 := ":" + ":" + "1"
	domain := "host" + ".example" + ".com"
	secret := "orch" + "id" + "82"
	second := "ma" + "ple" + "93"
	user := "ali" + "ce"
	lower := "ced" + "ar"
	upper := strings.ToUpper(lower)
	mixed := "Ce" + "DaR"
	symbols := "Pi" + "ne" + "@" + "!" + "#"
	firstPart := "pa" + "ss"
	lastPart := "wo" + "rd"
	port := "80" + "80"
	for _, tc := range []compactCredentialCase{
		{"A01_comma", ip + "," + secret, []string{ip, secret}},
		{"A02_wide_comma", ip + "，" + secret, []string{ip, secret}},
		{"A03_space", ip + " " + secret, []string{ip, secret}},
		{"A04_space_after", ip + ", " + secret, []string{ip, secret}},
		{"A04_space_before", ip + " ," + secret, []string{ip, secret}},
		{"A04_spaces_both", ip + " , " + secret, []string{ip, secret}},
		{"A05_semicolon", ip + ";" + secret, []string{ip, secret}},
		{"A05_wide_semicolon", ip + "；" + secret, []string{ip, secret}},
		{"A06_colon", ip + ":" + secret, []string{ip, secret}},
		{"A07_tab", ip + "\t" + secret, []string{ip, secret}},
		{"A08_newline", ip + "\n" + secret, []string{ip, secret}},
		{"A08_two_lines", ip + "," + secret + "\n" + ip2 + "," + second, []string{ip, secret, ip2, second}},
		{"A08_crlf", ip + "\r\n" + secret, []string{ip, secret}},
		{"B09_port", ip + ":22," + secret, []string{ip, secret}},
		{"B09_port_space", ip + ":" + port + " " + secret, []string{ip, secret}},
		{"B10_domain", domain + "," + secret, []string{secret}},
		{"B10_domain_colon", domain + ":" + secret, []string{secret}},
		{"B10_domain_port", domain + ":22," + secret, []string{secret}},
		{"B11_invalid_comma", invalid + "," + secret, nil},
		{"B11_invalid_wide_comma", invalid + "，" + secret, nil},
		{"B11_invalid_space", invalid + " " + secret, nil},
		{"B11_invalid_semicolon", invalid + ";" + secret, nil},
		{"B11_invalid_wide_semicolon", invalid + "；" + secret, nil},
		{"B11_invalid_colon", invalid + ":" + secret, nil},
		{"B11_invalid_tab", invalid + "\t" + secret, nil},
		{"B11_invalid_newline", invalid + "\n" + secret, nil},
		{"B12_cidr", ip + "/24," + secret, []string{ip}},
		{"B12_cidr_space", ip + "/24 " + secret, []string{ip}},
		{"B12_cidr_colon", ip + "/24:" + secret, []string{ip}},
		{"B12_ipv6_cidr", ip6 + "/64," + secret, []string{ip6}},
		{"B13_ipv6", ip6 + "," + secret, []string{ip6, secret}},
		{"B13_ipv6_space", ip6 + " " + secret, []string{ip6, secret}},
		{"B13_ipv6_short", short6 + "," + secret, []string{secret}},
		{"B13_ipv6_brackets", "[" + ip6 + "]," + secret, []string{ip6, secret}},
		{"B13_ipv6_port", "[" + ip6 + "]:22," + secret, []string{ip6, secret}},
		{"C14_comma_chain", ip + "," + user + "," + secret, []string{ip, user, secret}},
		{"C15_space_chain", ip + " " + user + " " + secret, []string{ip, user, secret}},
		{"C16_no_anchor", user + "," + secret, nil},
		{"C16_single", secret, nil},
		{"C17_reverse", secret + "," + ip, []string{ip}},
		{"D18_lower", ip + "," + lower, []string{ip, lower}},
		{"D18_upper", ip + "," + upper, []string{ip, upper}},
		{"D18_mixed", ip + "," + mixed, []string{ip, mixed}},
		{"D19_symbols", ip + "," + symbols, []string{ip, symbols}},
		{"D19_symbols_colon", ip + ":" + symbols, []string{ip, symbols}},
		{"D20_digits", ip + "," + port, []string{ip}},
		{"D20_symbols_only", ip + "," + "@!" + "#$", []string{ip}},
		{"D20_three_bytes", ip + "," + "a" + "12", []string{ip}},
		{"D20_four_bytes", ip + "," + "a" + "123", []string{ip, "a" + "123"}},
		{"D20_non_ascii_letters", ip + "," + "é" + "é", []string{ip}},
		{"D20_byte_length", ip + "," + "a" + "界", []string{ip, "a" + "界"}},
		{"D21_comma_parts", ip + "," + firstPart + "," + lastPart, []string{ip, firstPart, lastPart}},
		{"D21_short_tail", ip + "," + firstPart + "," + "xy", []string{ip, firstPart}},
		{"D22_repeat", ip + "," + secret + "\n再次 " + secret, []string{ip, secret}},
		{"D23_distinct", ip + "," + secret + ";" + ip2 + "," + second, []string{ip, secret, ip2, second}},
		{"E24_freight", "4550/" + "5750USD", nil},
		{"E24_transit", "30-" + "40天", nil},
		{"E25_amount", "1," + "234.56", nil},
		{"E25_coordinates", "29.123456" + "," + "119.456789", nil},
		{"E25_decimal", "123" + ".456", nil},
		{"E26_date", "2026-" + "11-24", nil},
		{"E26_time", "12:" + "30:45", nil},
		{"E26_version", "v2" + ".4.1", nil},
		{"E27_port_comma", ip + ", " + port, []string{ip}},
		{"E27_port_colon", ip + ":" + port, []string{ip}},
		{"E27_chinese", ip + ", " + "已经恢复了", []string{ip}},
		{"E27_chinese_wide_comma", ip + "，已经恢复了", []string{ip}},
		{"E28_prose", "服务器 " + ip + " 已经恢复了，请检查状态", []string{ip}},
		{"E28_adjacent_prose", "服务器" + ip + "已经恢复了", []string{ip}},
		{"E29_url", "https" + "://" + domain + "/status", nil},
		{"E29_url_host_followed", "https" + "://" + domain + "," + secret, nil},
		{"E29_email", user + "@" + domain, nil},
		{"E29_email_followed", user + "@" + domain + "," + secret, nil},
		{"E29_phone", "138" + "0013" + "8000", nil},
		{"E29_media_type", "application" + "/json", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCompactCredentialCase(t, tc, Options{ProtectIPs: true})
		})
	}
}

func TestTask27Baseline(t *testing.T) {
	ip := strings.Join([]string{"192", "0", "2", "27"}, ".")
	invalid := strings.Join([]string{"999", "0", "2", "27"}, ".")
	secret := "pa" + "ss" + "word"
	for _, tc := range []compactCredentialCase{
		{"R01_english_label", secret + ": " + secret, []string{secret}},
		{"R02_chinese_statement", "密" + "码是 " + secret, []string{secret}},
		{"R03_repeated_statement", "密" + "码是 " + secret + "，再次 " + secret, []string{secret}},
		{"R04_glued_chinese", "密" + "码" + secret, nil},
		{"R05_ipv4", ip, []string{ip}},
		{"R06_invalid_ipv4", invalid, nil},
		{"R07_redis_port", "redis 默认端口是 " + "63" + "79", nil},
		{"R08_operations", "已经恢复了，请检查服务状态", nil},
		{"R09_flag_separator", "运行 " + flagSlashText() + " 查看缓存", nil},
		{"R10_freight", "4550/" + "5750USD", nil},
		{"R11_transit", "30-" + "40天", nil},
		{"R12_amount", "1," + "234.56", nil},
		{"R13_coordinates", "29.123456" + "," + "119.456789", nil},
		{"R14_date", "2026-" + "11-24", nil},
		{"R15_time", "12:" + "30:45", nil},
		{"R16_version", "v2" + ".4.1", nil},
		{"R17_media_type", "application" + "/json", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCompactCredentialCase(t, tc, Options{ProtectIPs: true})
		})
	}
}

func TestCompactCredentialWithoutAddressMasking(t *testing.T) {
	ip := strings.Join([]string{"192", "0", "2", "28"}, ".")
	secret := "orch" + "id" + "82"
	assertCompactCredentialCase(t, compactCredentialCase{
		name: "credential_only", input: ip + "," + secret, want: []string{secret},
	}, Options{})
}

func TestCompactCredentialLimits(t *testing.T) {
	ip := strings.Join([]string{"192", "0", "2", "28"}, ".")
	for _, tc := range []struct {
		name       string
		candidate  string
		full       bool
		bestEffort bool
	}{
		{"oversized", strings.Repeat("x", MaxSecretBytes+1), false, false},
		{"full", "orch" + "id" + "82", true, false},
		{"site_wide_oversized", strings.Repeat("x", MaxSecretBytes+1), false, true},
		{"site_wide_full", "orch" + "id" + "82", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := NewSession()
			session.BestEffort = tc.bestEffort
			if tc.full {
				for i := 0; i < MaxSecrets; i++ {
					require.NoError(t, session.Add(fmt.Sprintf("existing-%d", i)))
				}
			}
			body, err := common.Marshal(map[string]string{"content": ip + "," + tc.candidate})
			require.NoError(t, err)
			protected, err := session.ProtectJSON(body, Options{})
			assert.ErrorIs(t, err, ErrLimit, "credential registration must retain Add limits")
			assert.True(t, protected == nil, "registration failure must not return unprotected JSON")
		})
	}
}
