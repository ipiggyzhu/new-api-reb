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
	// 上游模型 id 带有厂商前缀（形如 openai/gpt-4o）时，是否只把去掉前缀的
	// 名字（gpt-4o）加入渠道，并在渠道模型映射里写入 gpt-4o -> openai/gpt-4o，
	// 让请求自动转回上游的原名。默认开启。只处理斜杠前缀，日期后缀等保持原样。
	UpstreamModelUpdateStripVendorPrefix bool `json:"upstream_model_update_strip_vendor_prefix"`

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
	UpstreamModelUpdateStripVendorPrefix:       true,
}

// builtinChannelTestPrompts 是渠道测试与模型验证的默认提示词池：100 道 Java
// 面试八股文，每次测试随机抽一道。内容刻意贴近真实开发提问：固定的 "hi" 很
// 容易被上游判成机器人探测，进而触发风控或返回缓存响应，让测试结果失去意义。
// 补全会被 max_tokens 截断，这不影响判定（validateTestResponseBody 只检查错误
// 载荷、流事件和是否有实际输出），所以提示词长度只影响提示 token，成本可忽略。
var builtinChannelTestPrompts = []string{
	// Java 基础
	"Java 中 == 和 equals() 的区别是什么？重写 equals 为什么必须同时重写 hashCode？",
	"String、StringBuilder、StringBuffer 三者的区别和各自适用场景是什么？",
	"Java 的字符串常量池是怎么工作的？new String(\"abc\") 会创建几个对象？",
	"Integer 缓存是什么？Integer a = 127, b = 127 和 a = 128, b = 128 用 == 比较结果分别是什么，为什么？",
	"接口和抽象类有什么区别？Java 8 之后接口可以有默认方法，抽象类还有存在的必要吗？",
	"Java 中的重载和重写有什么区别？重写对访问修饰符、返回值和异常有什么要求？",
	"final 关键字作用在类、方法、变量上分别是什么含义？",
	"static 关键字有哪些用法？静态代码块、实例代码块和构造方法的执行顺序是什么？",
	"Java 的异常体系是怎样的？Error 和 Exception、受检异常和非受检异常有什么区别？",
	"try-catch-finally 中 finally 一定会执行吗？finally 里 return 会发生什么？",
	"Java 的泛型擦除是什么？为什么 List<String> 和 List<Integer> 运行时是同一个类型？",
	"泛型中的 ? extends T 和 ? super T 有什么区别？什么是 PECS 原则？",
	"Java 反射的原理是什么？反射为什么慢，有什么优化手段？",
	"Java 的深拷贝和浅拷贝有什么区别？如何实现深拷贝？",
	"Java 序列化是怎么回事？serialVersionUID 有什么作用？transient 关键字的作用是什么？",
	"Java Stream 的中间操作和终端操作有什么区别？Stream 是惰性求值的吗？",
	"Java 的自动装箱和拆箱是怎么实现的？有哪些容易踩的坑？",
	"Java 中 Object 类有哪些方法？wait/notify 为什么定义在 Object 而不是 Thread 上？",
	// 集合
	"ArrayList 和 LinkedList 的区别是什么？分别适合什么场景？",
	"ArrayList 的扩容机制是怎样的？初始容量和扩容倍数分别是多少？",
	"HashMap 的底层数据结构是什么？put 一个元素的完整流程是怎样的？",
	"HashMap 为什么在链表长度超过 8 时转为红黑树？为什么是 8 而不是其他数字？",
	"HashMap 的负载因子为什么是 0.75？扩容时元素是如何重新分布的？",
	"HashMap 在 JDK 1.7 和 1.8 中有哪些区别？1.7 多线程下为什么会形成死循环？",
	"HashMap 和 Hashtable、ConcurrentHashMap 的区别是什么？",
	"ConcurrentHashMap 在 JDK 1.8 中是怎么保证线程安全的？size() 是如何实现的？",
	"HashSet 是如何保证元素不重复的？它的底层是什么？",
	"LinkedHashMap 是怎么实现有序的？如何用它实现一个 LRU 缓存？",
	"TreeMap 的底层是什么？它对 key 有什么要求？",
	"Iterator 的 fail-fast 机制是什么？ConcurrentModificationException 是怎么产生的？",
	"CopyOnWriteArrayList 的实现原理是什么？适合什么场景，有什么缺点？",
	"Arrays.asList() 返回的 List 有什么坑？Collections.unmodifiableList 和 List.of 有什么区别？",
	// 并发
	"Java 线程有哪几种状态？它们之间是如何转换的？",
	"创建线程有哪几种方式？Runnable 和 Callable 有什么区别？",
	"synchronized 的底层实现原理是什么？锁升级的过程是怎样的？",
	"synchronized 和 ReentrantLock 有什么区别？各自适合什么场景？",
	"volatile 关键字的作用是什么？它能保证原子性吗？为什么？",
	"什么是 Java 内存模型（JMM）？happens-before 规则有哪些？",
	"什么是指令重排序？双重检查锁的单例为什么要加 volatile？",
	"CAS 是什么？它有哪些问题（ABA、自旋开销），如何解决？",
	"AQS（AbstractQueuedSynchronizer）的原理是什么？ReentrantLock 是怎么基于 AQS 实现的？",
	"ThreadLocal 的原理是什么？为什么会内存泄漏，如何避免？",
	"线程池的核心参数有哪些？一个任务提交到线程池后的执行流程是怎样的？",
	"线程池有哪几种拒绝策略？为什么不推荐用 Executors 创建线程池？",
	"线程池的核心线程数应该怎么设置？CPU 密集型和 IO 密集型有什么不同？",
	"CountDownLatch、CyclicBarrier、Semaphore 分别是什么，有什么区别？",
	"wait() 和 sleep() 有什么区别？notify() 和 notifyAll() 有什么区别？",
	"什么是死锁？产生死锁的四个必要条件是什么？如何排查和避免死锁？",
	"CompletableFuture 是什么？相比 Future 有什么优势？常用的组合方法有哪些？",
	"读写锁 ReentrantReadWriteLock 的原理是什么？什么是锁降级？StampedLock 有什么改进？",
	"什么是乐观锁和悲观锁？Java 中分别有哪些实现？",
	// JVM
	"JVM 的内存区域是怎么划分的？哪些是线程私有的，哪些是线程共享的？",
	"Java 对象的创建过程是怎样的？对象在内存中的布局是什么样的？",
	"如何判断一个对象可以被回收？引用计数法和可达性分析有什么区别？GC Roots 有哪些？",
	"Java 有哪几种引用类型？强引用、软引用、弱引用、虚引用分别在什么场景使用？",
	"常见的垃圾回收算法有哪些？标记清除、标记整理、复制算法各有什么优缺点？",
	"为什么要分代回收？新生代为什么要分为 Eden 和两个 Survivor 区？",
	"Minor GC、Major GC 和 Full GC 的区别是什么？什么情况下会触发 Full GC？",
	"CMS 收集器的工作过程是怎样的？它有什么缺点？",
	"G1 收集器的原理是什么？相比 CMS 有什么优势？什么是 Region 和 Remembered Set？",
	"ZGC 是如何做到低延迟的？什么是染色指针和读屏障？",
	"类加载的过程是怎样的？加载、验证、准备、解析、初始化各做了什么？",
	"什么是双亲委派模型？它的作用是什么？如何打破双亲委派？",
	"什么是 JIT 即时编译？什么是热点代码？逃逸分析能带来哪些优化？",
	"内存泄漏和内存溢出有什么区别？OutOfMemoryError 有哪些常见类型，如何排查？",
	"如何排查线上 CPU 飙高的问题？jstack、jmap、jstat 分别用来做什么？",
	"常用的 JVM 调优参数有哪些？-Xms、-Xmx、-Xmn、-XX:MetaspaceSize 分别是什么？",
	// Spring
	"Spring IoC 的原理是什么？Bean 的生命周期是怎样的？",
	"Spring AOP 的原理是什么？JDK 动态代理和 CGLIB 代理有什么区别？",
	"Spring 是如何解决循环依赖的？为什么需要三级缓存，二级缓存不够吗？",
	"Spring Bean 的作用域有哪些？单例 Bean 是线程安全的吗？",
	"@Autowired 和 @Resource 有什么区别？按类型注入和按名称注入的规则是什么？",
	"Spring 事务的传播行为有哪些？REQUIRED 和 REQUIRES_NEW 有什么区别？",
	"Spring 事务在什么情况下会失效？同一个类里方法自调用为什么事务不生效？",
	"Spring MVC 的请求处理流程是怎样的？DispatcherServlet 做了什么？",
	"Spring Boot 自动配置的原理是什么？@SpringBootApplication 包含了哪些注解？",
	"Spring Boot Starter 是怎么工作的？如何自定义一个 Starter？",
	"BeanFactory 和 ApplicationContext 有什么区别？",
	"BeanPostProcessor 和 BeanFactoryPostProcessor 有什么区别？各自的典型用途是什么？",
	"@Transactional 注解放在接口、类、方法上分别有什么效果？rollbackFor 的默认行为是什么？",
	// MySQL 与持久层
	"MySQL 的 InnoDB 和 MyISAM 有什么区别？",
	"MySQL 索引的底层数据结构是什么？为什么用 B+ 树而不是 B 树或哈希表？",
	"什么是聚簇索引和非聚簇索引？什么是回表？什么是覆盖索引？",
	"什么是最左前缀原则？联合索引 (a, b, c) 在哪些查询条件下能被使用？",
	"什么情况下索引会失效？如何用 EXPLAIN 分析一条慢 SQL？",
	"MySQL 的事务隔离级别有哪些？脏读、不可重复读、幻读分别是什么？",
	"MVCC 的原理是什么？undo log、Read View 和版本链是如何配合的？",
	"InnoDB 是如何解决幻读的？什么是间隙锁和临键锁？",
	"redo log、undo log 和 binlog 分别有什么作用？两阶段提交是怎么回事？",
	"MyBatis 中 #{} 和 ${} 有什么区别？MyBatis 的一级缓存和二级缓存是怎么工作的？",
	// Redis 与分布式
	"Redis 有哪些数据类型？各自的底层数据结构和典型应用场景是什么？",
	"Redis 为什么快？单线程模型是怎么回事？Redis 6.0 引入多线程改变了什么？",
	"Redis 的持久化方式有哪些？RDB 和 AOF 有什么区别？",
	"什么是缓存穿透、缓存击穿、缓存雪崩？分别如何解决？",
	"如何保证缓存和数据库的双写一致性？先删缓存还是先更新数据库？",
	"如何用 Redis 实现分布式锁？SETNX 有什么问题？Redisson 的看门狗机制是什么？",
	"什么是 CAP 定理和 BASE 理论？分布式事务有哪些解决方案（2PC、TCC、本地消息表、Seata）？",
	"消息队列如何保证消息不丢失、不重复消费、顺序消费？",
	"什么是幂等性？接口幂等有哪些实现方案？",
	"分布式 ID 有哪些生成方案？雪花算法的结构是什么，时钟回拨怎么处理？",
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
