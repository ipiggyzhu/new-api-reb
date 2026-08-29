package operation_setting

import (
	"os"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

type MonitorSetting struct {
	AutoTestChannelEnabled bool    `json:"auto_test_channel_enabled"`
	AutoTestChannelMinutes float64 `json:"auto_test_channel_minutes"`
	ChannelTestMode        string  `json:"channel_test_mode"`

	// 上游模型自动更新巡检。默认关闭：开启后按 UpstreamModelUpdateIntervalHours
	// 周期扫描未禁用渠道，对新增候选/到期复测/轮换抽查的模型发真实请求验证，
	// 只把验证通过的模型加入渠道，并删除连续失败达阈值的模型。
	UpstreamModelUpdateEnabled bool `json:"upstream_model_update_enabled"`
	// 巡检周期（小时）
	UpstreamModelUpdateIntervalHours int `json:"upstream_model_update_interval_hours"`
	// 是否忽略渠道级「检测上游模型更新」开关，扫描所有未禁用渠道
	UpstreamModelUpdateScanAllChannels bool `json:"upstream_model_update_scan_all_channels"`
	// 是否在加入模型前发真实请求验证
	UpstreamModelUpdateValidate bool `json:"upstream_model_update_validate"`
	// 是否删除连续验证失败达阈值的模型
	UpstreamModelUpdateRemoveFailed bool `json:"upstream_model_update_remove_failed"`
	// 是否把上游明确的「不支持该模型」（HTTP 404 且带模型级标记）计入模型失败。
	//
	// 默认关闭，且与 UpstreamModelUpdateRemoveFailed 是两道独立的闸门。原因是
	// 历史上这条判定走的是 service.IsChannelFaultError，而它的默认状态码白名单
	// 只有 401，所以 404「不支持所选模型」从来没有被计数过 —— 删除路径事实上
	// 是死的。修正判定会让一条既有部署里从未触发过的删除行为突然开始工作，即使
	// 管理员并没有改动任何设置。要求显式开启，是为了让这个行为变化是被选择的，
	// 而不是升级镜像的副作用。
	UpstreamModelUpdateRemoveUnavailableModels bool `json:"upstream_model_update_remove_unavailable_models"`
	// 模型验证失败后至少间隔多少分钟才复测
	UpstreamModelUpdateRetryDelayMinutes int `json:"upstream_model_update_retry_delay_minutes"`
	// 连续失败多少次才删除模型
	UpstreamModelUpdateFailureThreshold int `json:"upstream_model_update_failure_threshold"`
	// 每轮每个渠道对已有模型的轮换抽查数量
	UpstreamModelUpdateRotationSampleSize int `json:"upstream_model_update_rotation_sample_size"`
	// 每轮全局最多发起多少次模型验证请求（每次验证都会扣配额并写一条消费日志）
	UpstreamModelUpdateMaxValidationsPerRun int `json:"upstream_model_update_max_validations_per_run"`

	// 渠道测试提示词池。为空时回落到内置池，非空时只从其中随机抽取。
	// 渠道测试与模型验证共用，避免固定的 "hi" 被上游识别为机器人探测。
	ChannelTestPrompts []string `json:"channel_test_prompts"`

	// 渠道测试时附加到请求上的客户端请求头，覆盖按渠道类型内置的画像。
	//
	// 内置画像里的客户端版本号（claude-cli/x.y.z 之类）迟早会过时，而部分上游
	// 会按最低版本号拦截。写死在代码里意味着改一个字符串要重新构建镜像，所以
	// 留这个覆盖口。
	//
	// 外层键是客户端族："claude" / "openai" / "codex" / "gemini" / "generic"，
	// 以及 "*" 表示对所有族生效。**必须按族分开**：一张全局表会把 Claude Code
	// 的 user-agent 也套到 OpenAI 渠道上，那些渠道的测试会因此变成假失败。
	// 内层键为请求头名，值为请求头值，空值表示删除该内置头。
	// 优先级：渠道自身 header override > 本项族内配置 > 本项 "*" > 内置画像。
	ChannelTestClientHeaders map[string]map[string]string `json:"channel_test_client_headers"`
}

const (
	ChannelTestModeScheduledAll    = "scheduled_all"
	ChannelTestModePassiveRecovery = "passive_recovery"
)

// 默认配置
var monitorSetting = MonitorSetting{
	AutoTestChannelEnabled: false,
	AutoTestChannelMinutes: 10,
	ChannelTestMode:        ChannelTestModeScheduledAll,

	UpstreamModelUpdateEnabled:         false,
	UpstreamModelUpdateIntervalHours:   24,
	UpstreamModelUpdateScanAllChannels: true,
	UpstreamModelUpdateValidate:        true,
	UpstreamModelUpdateRemoveFailed:    true,
	// Off by default: see the field comment. Correcting the predicate would
	// otherwise start deleting models on deployments that never opted into it.
	UpstreamModelUpdateRemoveUnavailableModels: false,
	UpstreamModelUpdateRetryDelayMinutes:       60,
	UpstreamModelUpdateFailureThreshold:        2,
	UpstreamModelUpdateRotationSampleSize:      5,
	UpstreamModelUpdateMaxValidationsPerRun:    200,
}

// builtinChannelTestPrompts 是渠道测试与模型验证的默认提示词池。内容刻意贴近
// 真实开发提问：固定的 "hi" 很容易被上游判成机器人探测，进而触发风控或返回
// 缓存响应，让测试结果失去意义。补全会被 max_tokens 截断，这不影响判定
// （validateTestResponseBody 只检查错误载荷与流事件），所以提示词长度只影响
// 提示 token，成本可忽略。
var builtinChannelTestPrompts = []string{
	"用 HTML 和 JavaScript 写一个贪吃蛇小游戏，要求支持键盘控制和计分。",
	"液态玻璃（Liquid Glass）效果在 CSS 里怎么实现？给出关键属性和一个最小示例。",
	"解释一下数据库索引为什么能加快查询，什么情况下反而会变慢。",
	"如何用 CSS Grid 实现一个响应式的三栏布局？给出代码。",
	"写一个 Python 脚本，递归统计目录下各种文件类型的数量和总大小。",
	"Go 的 context 在什么场景下必须传递？举一个不传会出问题的例子。",
	"Implement a least-recently-used (LRU) cache in TypeScript with O(1) get and put.",
	"Explain the difference between TCP and UDP, and give one real-world use case for each.",
	"Write a SQL query that finds the second highest salary per department, and explain the window function you used.",
	"What causes a React component to re-render more often than expected, and how would you debug it?",
	"Design a rate limiter for an HTTP API. Compare token bucket and sliding window.",
	"用一段话解释什么是 CAP 定理，以及为什么分区容忍性通常不能放弃。",
}

// builtinChannelTestEmbeddingInputs 是 embedding 端点的内置输入池。
var builtinChannelTestEmbeddingInputs = []string{
	"数据库索引的工作原理",
	"how to implement an LRU cache",
	"responsive three column layout with css grid",
	"分布式系统中的一致性哈希",
}

// builtinChannelTestImagePrompts 是图像生成端点的内置提示词池。
var builtinChannelTestImagePrompts = []string{
	"a watercolor illustration of a lighthouse at dawn",
	"an isometric diagram of a small city park, flat design",
	"a close-up photo of morning dew on a spider web",
	"a minimalist poster about deep sea exploration",
}

// PickChannelTestPrompt 返回一条对话类测试提示词。管理员配置了提示词池时只从
// 配置中抽取，否则回落到内置池。
func PickChannelTestPrompt() string {
	return pickPrompt(normalizePromptPool(monitorSetting.ChannelTestPrompts), builtinChannelTestPrompts)
}

// PickChannelTestEmbeddingInput 返回一条 embedding 测试输入。
func PickChannelTestEmbeddingInput() string {
	return pickPrompt(nil, builtinChannelTestEmbeddingInputs)
}

// PickChannelTestImagePrompt 返回一条图像生成测试提示词。
func PickChannelTestImagePrompt() string {
	return pickPrompt(nil, builtinChannelTestImagePrompts)
}

// BuiltinChannelTestPrompts 返回内置对话提示词池的副本，供设置页填充示例。
func BuiltinChannelTestPrompts() []string {
	return append([]string(nil), builtinChannelTestPrompts...)
}

func pickPrompt(configured []string, builtin []string) string {
	pool := configured
	if len(pool) == 0 {
		pool = builtin
	}
	if len(pool) == 0 {
		return ""
	}
	return pool[common.GetRandomInt(len(pool))]
}

func normalizePromptPool(prompts []string) []string {
	normalized := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		if trimmed := strings.TrimSpace(prompt); trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	return normalized
}

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("monitor_setting", &monitorSetting)
}

// GetMonitorSetting returns a snapshot of the monitor settings with env
// overrides applied. It deliberately returns a copy: callers sit on hot paths
// (every relay request reads ChannelTestClientHeaders) and the upstream model
// scan runs channels concurrently, so writing env overrides into the shared
// registered struct here would be a data race with every other reader.
func GetMonitorSetting() *MonitorSetting {
	setting := monitorSetting
	if frequency, err := strconv.Atoi(os.Getenv("CHANNEL_TEST_FREQUENCY")); err == nil && frequency > 0 {
		setting.AutoTestChannelEnabled = true
		setting.AutoTestChannelMinutes = float64(frequency)
		setting.ChannelTestMode = ChannelTestModeScheduledAll
	}
	if enabled, ok := os.LookupEnv("CHANNEL_TEST_ENABLED"); ok {
		if parsed, err := strconv.ParseBool(enabled); err == nil {
			setting.AutoTestChannelEnabled = parsed
		}
	}
	if setting.ChannelTestMode != ChannelTestModePassiveRecovery {
		setting.ChannelTestMode = ChannelTestModeScheduledAll
	}
	return &setting
}

// SetMonitorSettingForTest replaces the registered monitor settings struct and
// returns a restore function. It exists because GetMonitorSetting hands out
// copies — precisely so no runtime caller can mutate shared state — which
// leaves tests in other packages no seam to install fixture settings.
func SetMonitorSettingForTest(setting MonitorSetting) (restore func()) {
	original := monitorSetting
	monitorSetting = setting
	return func() { monitorSetting = original }
}

// 上游模型巡检各数值项的兜底值。数据库里存的是 0 时（例如管理员清空了输入框）
// 直接使用会退化成"周期 0 小时""阈值 0 次立即删模型"，所以统一在读取处兜底。
const (
	defaultUpstreamModelUpdateIntervalHours        = 24
	defaultUpstreamModelUpdateRetryDelayMinutes    = 60
	defaultUpstreamModelUpdateFailureThreshold     = 2
	defaultUpstreamModelUpdateMaxValidationsPerRun = 200
)

// GetUpstreamModelUpdateIntervalHours 返回巡检周期，非正值回落默认 24 小时。
func (s *MonitorSetting) GetUpstreamModelUpdateIntervalHours() int {
	if s.UpstreamModelUpdateIntervalHours <= 0 {
		return defaultUpstreamModelUpdateIntervalHours
	}
	return s.UpstreamModelUpdateIntervalHours
}

// GetUpstreamModelUpdateRetryDelayMinutes 返回失败模型的复测间隔，非正值回落 60 分钟。
func (s *MonitorSetting) GetUpstreamModelUpdateRetryDelayMinutes() int {
	if s.UpstreamModelUpdateRetryDelayMinutes <= 0 {
		return defaultUpstreamModelUpdateRetryDelayMinutes
	}
	return s.UpstreamModelUpdateRetryDelayMinutes
}

// GetUpstreamModelUpdateFailureThreshold 返回删除模型所需的连续失败次数，
// 非正值回落 2：阈值 1 会让一次网络抖动就删掉模型。
func (s *MonitorSetting) GetUpstreamModelUpdateFailureThreshold() int {
	if s.UpstreamModelUpdateFailureThreshold <= 0 {
		return defaultUpstreamModelUpdateFailureThreshold
	}
	return s.UpstreamModelUpdateFailureThreshold
}

// GetUpstreamModelUpdateRotationSampleSize 返回每渠道轮换抽查数量，负值视为 0
// （0 是有效配置：只验证新增候选和到期复测，不做轮换抽查）。
func (s *MonitorSetting) GetUpstreamModelUpdateRotationSampleSize() int {
	if s.UpstreamModelUpdateRotationSampleSize < 0 {
		return 0
	}
	return s.UpstreamModelUpdateRotationSampleSize
}

// GetUpstreamModelUpdateMaxValidationsPerRun 返回单轮验证请求预算，非正值回落 200。
func (s *MonitorSetting) GetUpstreamModelUpdateMaxValidationsPerRun() int {
	if s.UpstreamModelUpdateMaxValidationsPerRun <= 0 {
		return defaultUpstreamModelUpdateMaxValidationsPerRun
	}
	return s.UpstreamModelUpdateMaxValidationsPerRun
}
