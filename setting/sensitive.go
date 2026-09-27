package setting

import "strings"

var CheckSensitiveEnabled = true
var CheckSensitiveOnPromptEnabled = true

// CheckSensitiveOnCompletionEnabled 是否检查上游返回的内容（防提示词注入）。
// 默认关闭：开启后每个响应体、流模式下每个分片都要多走一次 JSON 遍历和敏感词
// 匹配，而且误报会直接掐断一个已经计费的请求。这种代价必须由管理员显式承担，
// 而不是升级后悄悄生效。
var CheckSensitiveOnCompletionEnabled = false

// StopOnSensitiveEnabled 如果检测到敏感词，是否立刻停止生成，否则替换敏感词
var StopOnSensitiveEnabled = true

// StreamCacheQueueLength 流模式缓存队列长度，0表示无缓存
var StreamCacheQueueLength = 0

// SensitiveWords 敏感词
// var SensitiveWords []string
var SensitiveWords = []string{
	"test_sensitive",
}

func SensitiveWordsToString() string {
	return strings.Join(SensitiveWords, "\n")
}

func SensitiveWordsFromString(s string) {
	SensitiveWords = []string{}
	sw := strings.Split(s, "\n")
	for _, w := range sw {
		w = strings.TrimSpace(w)
		if w != "" {
			SensitiveWords = append(SensitiveWords, w)
		}
	}
}

func ShouldCheckPromptSensitive() bool {
	return CheckSensitiveEnabled && CheckSensitiveOnPromptEnabled
}

func ShouldCheckCompletionSensitive() bool {
	return CheckSensitiveEnabled && CheckSensitiveOnCompletionEnabled
}
