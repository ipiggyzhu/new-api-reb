package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/QuantumNous/new-api/common"
)

const (
	credentialWords        = `[a-z0-9_-]*(?:password|passwd|pwd|passphrase|secret|api[ _-]?key|token)`
	chineseCredentialWords = `密码|密碼|口令|密钥|秘钥|令牌|验证码|驗證碼`
	quotedValue            = "\"([^\"\\r\\n]+)\"|'([^'\\r\\n]+)'|`([^`\\r\\n]+)`"
	writtenValue           = `(?:` + quotedValue + "|([^\\s,;，；。<>`\"']+))"
	// A value typed straight after a Chinese word ends where the Chinese resumes.
	asciiValue = `(?:` + quotedValue + "|([^\\s,;:=<>`\"'\\x{80}-\\x{10FFFF}][^\\s,;<>`\"'\\x{80}-\\x{10FFFF}]*))"
	shellValue = `(?:"([^"\r\n]+)"|'([^'\r\n]+)'|([^\s"'|;&]+))`
)

var (
	credentialLabel      = regexp.MustCompile(`(?i)(?:password|passwd|pwd|passphrase|secret|api[_-]?key|token|authorization|cookie|private[_-]?key)$`)
	credentialAssignment = regexp.MustCompile(`(?i)(?:` + credentialWords + `|` + chineseCredentialWords + `)["']?\s*[:=：]\s*` + writtenValue)
	// "密码是 admin" names a value; "密码是什么" asks a question.
	credentialStatement = regexp.MustCompile(`(?i)(?:` + credentialWords + `|` + chineseCredentialWords + `)\s*(?:是|为)\s*` + writtenValue)
	// Without a separator the neighbour is usually prose: "my password is
	// hunter2" and "登录密码 Abc123" versus "password is required".
	credentialMention = regexp.MustCompile(`(?i)(?:` + credentialWords + `\s+(?:is|are|was)\s*[:：]?\s+` + writtenValue + `|(?:` + chineseCredentialWords + `)\s*` + asciiValue + `)`)
	// Client programs that take the credential glued to, or right after, a flag.
	commandLineCredential = regexp.MustCompile(`\b(?:(?i:mysql\w*|mariadb[\w-]*|mycli)\b[^\r\n|;&]*?\s-p|(?i:sshpass)\b[^\r\n|;&]*?\s-p\s*|(?i:redis-cli)\b[^\r\n|;&]*?\s(?:-a|--pass)\s+|(?i:curl)\b[^\r\n|;&]*?\s(?:-u|--user)[\s=]+["']?[^\s:"']+:)` + shellValue)
	asciiAlphanumeric     = regexp.MustCompile(`[A-Za-z0-9]`)
	credentialPatterns    = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----[\s\S]+?-----END (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`),
		regexp.MustCompile(`\b(?:sk-(?:proj-|ant-api\d+-)?[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[A-Z0-9]{16})\b`),
	}
	bearerPattern      = regexp.MustCompile(`(?i)\bBearer[ \t]+([A-Za-z0-9._~+/-]+=*)`)
	urlPasswordPattern = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]+:([^\s/@]+)@`)
	ipv4Pattern        = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	ipv6Pattern        = regexp.MustCompile(`(?:[a-fA-F0-9]{0,4}:){2,}[a-fA-F0-9]{0,4}`)
)

type Options struct {
	ProtectIPs   bool
	RestoreTools bool
}

// A dotted DNS name must have a letter-led final label, so malformed IPv4,
// versions and decimal numbers cannot become credential anchors.
var credentialDomainPattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.?$`)

func compactCredentialAnchor(value string) bool {
	if host, port, err := net.SplitHostPort(value); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		value = host
	} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = value[1 : len(value)-1]
	}
	return net.ParseIP(value) != nil || len(value) <= 253 && credentialDomainPattern.MatchString(value)
}

// walkJSON preserves numeric lexemes (including large integers), explicit
// zeros, unknown fields, and nested JSON tool arguments. Errors never echo input.
func walkJSON(data []byte, key string, depth int, fn func(string, string) (string, error)) ([]byte, error) {
	if depth > 48 {
		return nil, ErrLimit
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("invalid privacy JSON")
	}
	switch data[0] {
	case '{':
		var object map[string]json.RawMessage
		if common.Unmarshal(data, &object) != nil {
			return nil, errors.New("invalid privacy JSON")
		}
		// The request itself is never opaque, whatever its own "type" says.
		if depth > 0 && opaqueObject(key, object) {
			return data, nil
		}
		for name, value := range object {
			if opaqueField(name, value) {
				continue
			}
			changed, err := walkJSON(value, name, depth+1, fn)
			if err != nil {
				return nil, err
			}
			object[name] = changed
		}
		return common.Marshal(object)
	case '[':
		var array []json.RawMessage
		if common.Unmarshal(data, &array) != nil {
			return nil, errors.New("invalid privacy JSON")
		}
		for i := range array {
			changed, err := walkJSON(array[i], key, depth+1, fn)
			if err != nil {
				return nil, err
			}
			array[i] = changed
		}
		return common.Marshal(array)
	case '"':
		var value string
		if common.Unmarshal(data, &value) != nil {
			return nil, errors.New("invalid privacy JSON")
		}
		trimmed := strings.TrimSpace(value)
		// An inline upload is encoded bytes, not text: a short secret can occur
		// in it by chance and replacing it there would corrupt the file.
		if strings.HasPrefix(trimmed, "data:") && strings.Contains(trimmed[:min(len(trimmed), 128)], ";base64,") {
			return data, nil
		}
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var raw json.RawMessage
			if common.Unmarshal([]byte(trimmed), &raw) == nil {
				changed, err := walkJSON(raw, key, depth+1, fn)
				if err != nil {
					return nil, err
				}
				value = string(changed)
			}
		}
		value, err := fn(key, value)
		if err != nil {
			return nil, err
		}
		return common.Marshal(value)
	default:
		var raw json.RawMessage
		if common.Unmarshal(data, &raw) != nil {
			return nil, errors.New("invalid privacy JSON")
		}
		return data, nil
	}
}

func (s *Session) discover(key, value string, options Options) (string, error) {
	if credentialLabel.MatchString(key) || key == "密码" || key == "密钥" || key == "口令" {
		if err := s.Add(value); err != nil {
			return "", err
		}
	}
	for _, pattern := range credentialPatterns {
		for _, match := range pattern.FindAllString(value, -1) {
			if err := s.Add(match); err != nil {
				return "", err
			}
		}
	}
	for _, rule := range []struct {
		pattern *regexp.Regexp
		accept  func(string) bool
	}{
		{credentialAssignment, nil},
		{bearerPattern, nil},
		{urlPasswordPattern, nil},
		{commandLineCredential, nil},
		{credentialStatement, asciiAlphanumeric.MatchString},
		{credentialMention, plausibleSecret},
	} {
		for _, match := range rule.pattern.FindAllStringSubmatch(value, -1) {
			for _, capture := range match[1:] {
				// Sentence punctuation is not part of the value, and later
				// repetitions (tool output, the next turn) come without it.
				bare := strings.TrimRight(capture, ".!?)")
				if capture == "" || rule.accept != nil && !rule.accept(bare) {
					continue
				}
				if err := s.Add(capture); err != nil {
					return "", err
				}
				if len(bare) >= 4 {
					if err := s.Add(bare); err != nil {
						return "", err
					}
				}
			}
		}
	}
	// Compact lists have an address or hostname first. Keep IPv6 and numeric
	// ports intact before interpreting colons as credential separators.
	anchored := false
	for _, field := range strings.FieldsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，;；", r)
	}) {
		if _, _, err := net.ParseCIDR(field); err == nil {
			anchored = false
			continue
		}
		for end := len(field); end > 0; end = strings.LastIndexByte(field[:end], ':') {
			// DNS names have at most 253 bytes, plus a colon and five port
			// digits. Bound prefix validation even for very long captures.
			if end <= 259 && compactCredentialAnchor(field[:end]) {
				anchored = true
				field = strings.TrimPrefix(field[end:], ":")
				break
			}
		}
		if !anchored {
			continue
		}
		for candidate := range strings.SplitSeq(field, ":") {
			if candidate == "" {
				continue
			}
			if !strings.ContainsAny(candidate, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
				// Prose ends the list, while numeric or symbolic fields only
				// skip registration; later credentials still belong to it.
				if strings.ContainsFunc(candidate, unicode.IsLetter) {
					anchored = false
					break
				}
				continue
			}
			// Add retains the shared byte-length, plausibility and capacity
			// gates. No capture spans whitespace or a list separator.
			if err := s.Add(candidate); err != nil {
				return "", err
			}
		}
	}
	if options.ProtectIPs {
		if err := s.addAddresses(ipAddresses(value)); err != nil {
			return "", err
		}
	}
	return value, nil
}

func ipAddresses(text string) []string {
	var found []string
	for _, pattern := range []*regexp.Regexp{ipv4Pattern, ipv6Pattern} {
		for _, match := range pattern.FindAllString(text, -1) {
			if net.ParseIP(match) != nil {
				found = append(found, match)
			}
		}
	}
	return found
}

// addAddresses stores IP addresses. Under the site-wide switch a pasted log
// with more addresses than a session holds keeps the ones that fit instead of
// failing every later request of that conversation.
func (s *Session) addAddresses(addresses []string) error {
	for _, address := range addresses {
		if err := s.Add(address); err != nil {
			if s.BestEffort && errors.Is(err, ErrLimit) {
				return nil
			}
			return err
		}
	}
	return nil
}

// plausibleSecret tells a credential mentioned in passing from the ordinary
// word that follows the same label. Every stored value is replaced wherever it
// occurs, so a common word must never be stored.
func plausibleSecret(value string) bool {
	if len(value) < 4 {
		return false
	}
	var lower, innerUpper bool
	for i, r := range value {
		switch {
		case r >= '0' && r <= '9':
			return true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			innerUpper = innerUpper || i > 0
		case r > ' ' && r < 0x7f && !strings.ContainsRune("-_./", r):
			return true
		}
	}
	return lower && innerUpper
}

func (s *Session) ProtectText(key, value string, options Options) (string, error) {
	if _, err := s.discover(key, value, options); err != nil {
		return "", err
	}
	return s.Mask(value), nil
}

func (s *Session) ProtectQuery(raw string, options Options) (string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return s.ProtectText("url", raw, options)
	}
	for key, entries := range values {
		for i, value := range entries {
			entries[i], err = s.ProtectText(key, value, options)
			if err != nil {
				return "", err
			}
		}
		values[key] = entries
	}
	return values.Encode(), nil
}

func (s *Session) ProtectJSON(data []byte, options Options) ([]byte, error) {
	// Discover before replacing: a credential declared later must also disappear
	// from earlier history, tool results, URLs and repetitions without labels.
	// Credentials claim their references first, so that addresses met earlier in
	// the conversation cannot use up the room a password needs.
	var addresses []string
	discovered, err := walkJSON(data, "", 0, func(key, value string) (string, error) {
		if options.ProtectIPs {
			addresses = append(addresses, ipAddresses(value)...)
		}
		return s.discover(key, value, Options{})
	})
	clear(discovered)
	if err == nil {
		err = s.addAddresses(addresses)
	}
	if err != nil {
		return nil, err
	}
	return walkJSON(data, "", 0, func(key string, value string) (string, error) {
		if protocolField(key) {
			return value, nil
		}
		return s.Mask(value), nil
	})
}

func protocolField(key string) bool {
	switch key {
	case "model", "role", "type", "object", "id", "call_id", "tool_call_id", "name", "status", "finish_reason":
		return true
	}
	return false
}

func (s *Session) RestoreJSON(data []byte, restoreTools bool) ([]byte, error) {
	return s.restoreResponse(data, restoreTools, 0)
}

func (s *Session) restoreResponse(data []byte, restoreTools bool, depth int) ([]byte, error) {
	if depth > 48 {
		return nil, ErrLimit
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("invalid privacy response")
	}
	if data[0] == '{' {
		var object map[string]json.RawMessage
		if common.Unmarshal(data, &object) != nil {
			return nil, errors.New("invalid privacy response")
		}
		kind := common.JsonRawMessageToString(object["type"])
		if !restoreTools && (kind == "tool_use" || kind == "function_call" || kind == "custom_tool_call" || strings.Contains(kind, "function_call_arguments") || strings.Contains(kind, "custom_tool_call_input")) {
			return data, nil
		}
		for key, value := range object {
			if !restoreTools && (key == "tool_calls" || key == "function_call" || key == "functionCall" || key == "partial_json") {
				continue
			}
			if protocolField(key) {
				continue
			}
			changed, err := s.restoreResponse(value, restoreTools, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = changed
		}
		return common.Marshal(object)
	}
	if data[0] == '[' {
		var array []json.RawMessage
		if common.Unmarshal(data, &array) != nil {
			return nil, errors.New("invalid privacy response")
		}
		for i := range array {
			changed, err := s.restoreResponse(array[i], restoreTools, depth+1)
			if err != nil {
				return nil, err
			}
			array[i] = changed
		}
		return common.Marshal(array)
	}
	return walkJSON(data, "", depth, func(_ string, value string) (string, error) { return s.Restore(value), nil })
}

// opaqueObject and opaqueField mark what this layer never rewrites: media and
// file parts it cannot read, reasoning the provider encrypted itself, and
// provider-side tools whose settings must stay usable. Such content passes
// through byte for byte; a strict request that carries an upload or a
// provider-side tool is refused instead (see ValidateTextRequest).
func opaqueObject(parentKey string, object map[string]json.RawMessage) bool {
	kind := common.JsonRawMessageToString(object["type"])
	switch kind {
	case "image", "image_url", "input_image", "input_audio", "audio", "video", "input_video", "document", "file", "input_file", "computer", "computer_20250124":
		return true
	}
	return parentKey == "tools" && kind != "" && kind != "function" && kind != "custom"
}

func opaqueField(key string, value json.RawMessage) bool {
	switch key {
	case "inlineData", "inline_data", "fileData", "file_data", "mcp_servers", "encrypted_content", "image", "image_url", "audio", "audio_url", "input_audio", "input_image", "input_video", "video_url", "file":
		return string(value) != "null" && string(value) != "[]" && string(value) != `""`
	}
	return false
}

// ValidateTextRequest keeps a strict (per-request) promise honest: uploads and
// provider-side tools cannot be inspected, so they are refused rather than sent
// under a false promise of protection. A text secret is substituted, never
// rejected. Encrypted reasoning comes back from the provider and is allowed.
func ValidateTextRequest(data []byte) error {
	return validateText(data, "", 0)
}

func validateText(data []byte, key string, depth int) error {
	if depth > 48 {
		return ErrLimit
	}
	switch common.GetJsonType(data) {
	case "object":
		var object map[string]json.RawMessage
		if common.Unmarshal(data, &object) != nil {
			return errors.New("invalid privacy JSON")
		}
		if opaqueObject(key, object) {
			return ErrUnsupported
		}
		for name, value := range object {
			if name == "encrypted_content" {
				continue
			}
			if opaqueField(name, value) {
				return ErrUnsupported
			}
			if err := validateText(value, name, depth+1); err != nil {
				return err
			}
		}
	case "array":
		var array []json.RawMessage
		if common.Unmarshal(data, &array) != nil {
			return errors.New("invalid privacy JSON")
		}
		for _, value := range array {
			if err := validateText(value, key, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
