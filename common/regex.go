package common

import (
	"regexp"
	"sync"
)

// matchAnyRegexCache memoizes compiled patterns across every caller of
// MatchAnyRegex. Patterns come from configuration (channel-affinity rules,
// chat-to-responses conversion policies), so the key space is bounded by the
// admin's config rather than by request traffic.
var matchAnyRegexCache sync.Map // map[string]*regexp.Regexp

// MatchAnyRegex reports whether s matches any of the patterns. An empty pattern
// list and an empty s never match, and a pattern that fails to compile is skipped
// rather than aborting the check, so invalid config can never break live routing.
func MatchAnyRegex(patterns []string, s string) bool {
	if len(patterns) == 0 || s == "" {
		return false
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		re, ok := matchAnyRegexCache.Load(pattern)
		if !ok {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				continue
			}
			re = compiled
			matchAnyRegexCache.Store(pattern, re)
		}
		if re.(*regexp.Regexp).MatchString(s) {
			return true
		}
	}
	return false
}
