package service

import (
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/setting"

	"github.com/tidwall/gjson"
)

// Sensitive-word scanning of an upstream response has to read the decoded text,
// not the bytes on the wire. A JSON encoder may emit any character as a \uXXXX
// escape — Go's own encoder does it for <, > and & by default, and several
// upstreams escape all non-ASCII — so an Aho-Corasick search over the raw body
// silently misses every word the upstream happened to escape, and misses CJK
// words entirely against an ASCII-escaping upstream. Walking the document and
// searching each decoded string value is what closes that gap.
//
// Values are scanned, keys are not: injected prose and exfiltrated payloads live
// in values, while keys are schema names an upstream does not choose per
// request, so scanning them only invents false positives.

// maxSensitiveScanDepth bounds recursion on an upstream-controlled document. A
// response is not trusted input, and nothing legitimate nests this deep.
const maxSensitiveScanDepth = 64

// visitJSONString receives one decoded string value. Returning false stops the
// walk.
type visitJSONString func(value string) bool

func walkJSONStrings(res gjson.Result, depth int, visit visitJSONString) bool {
	if depth > maxSensitiveScanDepth {
		return true
	}
	switch res.Type {
	case gjson.String:
		return visit(res.String())
	case gjson.JSON:
		keepGoing := true
		res.ForEach(func(_, value gjson.Result) bool {
			keepGoing = walkJSONStrings(value, depth+1, visit)
			return keepGoing
		})
		return keepGoing
	}
	return true
}

// ScanResponseForSensitiveWords reports whether an upstream payload carries any
// configured sensitive word, returning the words that matched. It accepts both
// a whole non-stream response body and a single SSE chunk; a payload that is not
// a JSON document is scanned as raw text so a plain-text upstream error or an
// off-spec chunk is still covered.
func ScanResponseForSensitiveWords(payload []byte) (bool, []string) {
	if len(setting.SensitiveWords) == 0 || len(payload) == 0 {
		return false, nil
	}

	root := gjson.ParseBytes(payload)
	if !root.IsObject() && !root.IsArray() {
		return SensitiveWordContains(string(payload))
	}

	var (
		found []string
		seen  = make(map[string]struct{})
	)
	walkJSONStrings(root, 0, func(value string) bool {
		contains, words := SensitiveWordContains(value)
		if !contains {
			return true
		}
		for _, w := range words {
			if _, dup := seen[w]; dup {
				continue
			}
			seen[w] = struct{}{}
			found = append(found, w)
		}
		// One hit already decides the response, so the walk stops here instead
		// of scanning the rest of a body that is going to be rejected.
		return false
	})

	return len(found) > 0, found
}

// DescribeSensitiveWords renders matched words for a log line or an error
// message, capped so a dictionary-wide match cannot write an unbounded string
// into the logs or hand the caller a reflected copy of the whole word list.
func DescribeSensitiveWords(words []string) string {
	const maxReported = 5
	if len(words) == 0 {
		return ""
	}
	if len(words) <= maxReported {
		return strings.Join(words, ", ")
	}
	return strings.Join(words[:maxReported], ", ") + " (+" + strconv.Itoa(len(words)-maxReported) + " more)"
}
