package router

// ZCode Coding Plan（Anthropic Messages 网关）上游支持。
//
// 该网关的常量、头契约与 id 派生规则 1:1 移植自 dsh-zhipu-provider 的
// src/zcode.ts / src/ids.ts（后者又复制自参考客户端
// zcode_coding_plan_client.py 的 _get_base_headers）。这些值是上游自己的
// 合同，不是本项目的命名：网关路径、客户端版本、anthropic-version 一旦
// 上游变更必须同步更新，不能顺手改名。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
)

// ZCode 平台常量（上游合同，勿改名）。
const (
	ZcodeDefaultOrigin    = "https://zcode.z.ai"
	ZcodeClientVersion    = "3.17.1"
	ZcodeAnthropicVersion = "2023-06-01"

	ZcodeChannelBigmodel = "bigmodel"
	ZcodeChannelZai      = "zai"
)

// ZCode CLI 固定客户端身份值，逐字复刻参考客户端。
const (
	zcodeReferer         = "https://zcode.z.ai"
	zcodeTitle           = "Z Code@electron"
	zcodeReleaseChannel  = "production"
	zcodeAgent           = "glm"
	zcodeClientLanguage  = "zh-CN"
	zcodeClientTimezone  = "Asia/Shanghai"
	zcodeSessionTypeMain = "main"
)

// zcodeGatewayPath 返回渠道对应的 Messages 网关路径（不带前导斜杠，
// 与 base_url 拼接语义一致）。渠道是凭据的属性：同一批模型 ID 在两个
// 渠道上都能服务，路径随渠道切换。
func zcodeGatewayPath(channel string) string {
	if channel == ZcodeChannelZai {
		return "api/v1/ultra-zai/anthropic/v1/messages"
	}
	return "api/v1/ultra/anthropic/v1/messages"
}

// zcodeMessagesPath 返回候选实际使用的上游路径：显式路径覆盖 > 渠道网关路径 > 通用默认。
func zcodeMessagesPath(u Candidate) string {
	if u.AnthropicMessagesPath != "" {
		return strings.TrimLeft(u.AnthropicMessagesPath, "/")
	}
	if u.SimulateZcode {
		return zcodeGatewayPath(u.ZcodeChannel)
	}
	return "v1/messages"
}

/* -------------------------------------------------------------------------- */
/* 关联 id：稳定会话 id + 每请求新 id（移植自 ids.ts）                          */
/* -------------------------------------------------------------------------- */

// zcodeStableID 计算 sha256("prefix\x00value") 并截断为 12 字节：
// 同一会话稳定、不可逆、跨会话互异。
func zcodeStableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

func zcodeRandomID(prefix string, size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		// 随机源失败时退化到 newID()：id 仍需唯一，只是熵变弱。
		return prefix + "_" + newID() + newID()
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// zcodeConversationSeed 取第一条 user 消息 content 的 JSON 作为会话信号：
// 多轮对话随历史增长保持稳定，首 turn 不同则互异，且不可逆推用户文本。
// goccy 的 Marshal 对 map 键排序与标准库一致，因此同一 content 的序列化
// 在多次请求之间是逐字节稳定的。
func zcodeConversationSeed(messages []any) string {
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "user" {
			continue
		}
		content, exists := message["content"]
		if !exists || content == nil {
			continue
		}
		encoded, err := json.Marshal(content)
		if err != nil || len(encoded) == 0 {
			continue
		}
		if s := string(encoded); s != "null" {
			return s
		}
	}
	return ""
}

type zcodeRequestIDs struct {
	session string
	request string
	trace   string
}

// deriveZcodeRequestIDs 派生一次上游请求的关联 id。会话 id 来自对话本身
// （首个 user turn），请求/追踪 id 每次全新——与 ids.ts 的 deriveRequestIDs 一致。
func deriveZcodeRequestIDs(messages []any) zcodeRequestIDs {
	seed := zcodeConversationSeed(messages)
	if seed == "" || seed == "{}" {
		seed = zcodeRandomID("fallback", 16)
	}
	return zcodeRequestIDs{
		session: zcodeStableID("ses", seed),
		request: zcodeRandomID("req", 16),
		trace:   zcodeRandomID("trace", 16),
	}
}

/* -------------------------------------------------------------------------- */
/* CLI 身份头集                                                                */
/* -------------------------------------------------------------------------- */

// zcodeClientIdentityHeaders 复刻官方 zcode-source-headers.ts 的身份与
// 平台指纹头。语言与时区在 CLI 中是硬编码而非派生的，这里同样
// 硬编码：该头集的目的是复现桌面客户端指纹，派生值反而会失配。
// 平台名、OS 类别与 OS 版本在官方实现里是运行时派生的，这里同样派生。
// X-Device-Mid 由调用方按请求注入（见 zcodeDeviceHeaders），不属于
// 静态头集——官方同样只在设备身份存在时才发。
func zcodeClientIdentityHeaders() map[string]string {
	osName := zcodePlatformName()
	headers := map[string]string{
		"HTTP-Referer":         zcodeReferer,
		"X-Title":              zcodeTitle,
		"X-ZCode-App-Version":  ZcodeClientVersion,
		"X-Release-Channel":    zcodeReleaseChannel,
		"X-Client-Language":    zcodeClientLanguage,
		"X-Client-Timezone":    zcodeClientTimezone,
		"X-ZCode-Agent":        zcodeAgent,
		"X-Platform":           osName + "-" + zcodeArchName(),
		"X-Os-Category":        zcodeOSCategory(osName),
		"X-Os-Version":         zcodeOSVersion(),
		"x-zcode-session-type": zcodeSessionTypeMain,
		"anthropic-version":    ZcodeAnthropicVersion,
	}
	return headers
}

// zcodeDeviceHeaders 返回设备身份头（X-Device-Mid）。官方只在 deviceMid
// 存在时附加该头（buildZCodeSourceHeadersFromContext 的条件展开），请求
// 路径本身不依赖它——缺失时不发，与官方一致，不伪造身份。
func zcodeDeviceHeaders(deviceMid string) map[string]string {
	if deviceMid == "" {
		return nil
	}
	return map[string]string{"X-Device-Mid": deviceMid}
}

func zcodePlatformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin"
	case "windows":
		return "win32"
	default:
		return "linux"
	}
}

func zcodeArchName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	default:
		return runtime.GOARCH
	}
}

func zcodeOSCategory(osName string) string {
	switch osName {
	case "darwin":
		return "macos"
	case "win32":
		return "windows"
	default:
		return "linux"
	}
}

/* -------------------------------------------------------------------------- */
/* 凭据回退：环境变量之外读取 token 文件                                        */
/* -------------------------------------------------------------------------- */

const (
	// nanorouter 自己的 login-zcode 子命令写入的凭据文件。
	zcodeTokenStoreName = "zcode_token.json"
	// 参考客户端（zcode_coding_plan_client.py / dsh 插件）的凭据文件，
	// 这里只读不写：两个程序共享一个存储，写入会竞态。
	zcodeReferenceTokenName = ".zcode_coding_plan_token.json"
)

// zcodeTokenCache 缓存 token 文件的解析结果。参考客户端每次登录才重写
// 文件且 key 有效期 30 天，而请求路径每次环境变量未命中都会来读，短缓存
// 去掉了每次请求的文件读取成本；读取结果（含空）整体缓存，与 dsh 插件的
// cachedStoreReader 语义一致。
var zcodeTokenCache struct {
	sync.Mutex
	at    time.Time
	value map[string]string
}

// zcodeTokenStoreTTL 是凭据文件缓存的新鲜窗口。
const zcodeTokenStoreTTL = 5 * time.Second

func zcodeHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// ZcodeTokenStorePath 返回 login-zcode 凭据文件路径。
func ZcodeTokenStorePath() string {
	home := zcodeHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".nanorouter", zcodeTokenStoreName)
}

// ZcodeReferenceTokenPath 返回参考客户端凭据文件路径（只读）。
func ZcodeReferenceTokenPath() string {
	home := zcodeHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, zcodeReferenceTokenName)
}

// resolveCandidateAPIKey 解析候选凭据。顺序为环境变量（含智谱互备名）→
// 数据库 → 凭据文件。
//
// 数据库排在文件之前是刻意的：文件里的 key 只能由 login zcode 手工写入，
// 而 Secret/挂载卷在容器内只读，OAuth 刷新的新 token 无处可存。数据库
// 是程序自己能写的地方，登录完成后写库，后续请求直接读它。
func (r *Router) resolveCandidateAPIKey(ctx context.Context, u Candidate) string {
	if key := resolveAPIKey(u.APIKeyEnv); key != "" {
		return key
	}
	if !u.ZcodeTokenFile {
		return ""
	}
	if r.zcodeCredentials != nil {
		if c, ok := r.zcodeCredentials.get(ctx, u.ZcodeChannel); ok {
			return c.APIKey
		}
	}
	return zcodeTokenFileKey(u.ZcodeChannel)
}

// zcodeTokenFileKey 从凭据文件按渠道解析 Coding Plan key。三个只读来源
// 依次为：login zcode 写入的凭据文件、ZCode 官方加密凭据文件
// (~/.zcode/v2/credentials.json)、参考客户端的旧 token 文件；前者命中后
// 不再向后拼接，避免把两次登录的凭据混在一台机器上。文件内分组键缺失时
// 回退另一分组（dsh 的 grouped[provider] ?? bigmodel ?? zai 语义）——
// 切换渠道的用户不应静默丢失凭据。
func zcodeTokenFileKey(channel string) string {
	if channel != ZcodeChannelZai {
		channel = ZcodeChannelBigmodel
	}
	zcodeTokenCache.Lock()
	defer zcodeTokenCache.Unlock()
	now := time.Now()
	if zcodeTokenCache.value != nil && now.Sub(zcodeTokenCache.at) < zcodeTokenStoreTTL {
		return zcodeTokenCache.value[channel]
	}
	entries := map[string]string{}
	if path := ZcodeTokenStorePath(); path != "" {
		for name, key := range parseZcodeTokenStoreFile(path) {
			entries[name] = key
		}
	}
	// ZCode 官方 v2 凭据文件次之：机器上跑着 ZCode 桌面/CLI 时无需重复
	// 登录即可复用其已登录凭据（加密文件按同机默认密钥解密）。
	if path := ZcodeV2CredentialsPath(); path != "" {
		for name, key := range parseZcodeV2CredentialsFile(path) {
			if _, ok := entries[name]; !ok {
				entries[name] = key
			}
		}
	}
	if path := ZcodeReferenceTokenPath(); path != "" {
		for name, key := range parseZcodeTokenStoreFile(path) {
			if _, ok := entries[name]; !ok {
				entries[name] = key
			}
		}
	}
	zcodeTokenCache.at = now
	zcodeTokenCache.value = entries
	if key := entries[channel]; key != "" {
		return key
	}
	// 跨分组回退：bigmodel 优先于 zai，与请求路径的解析顺序一致。
	// 权衡（与 dsh 语义一致）：这会把 A 渠道签发的 key 发往 B 渠道的
	// 网关——功能上必然 401，但活体凭据会出现在对端访问日志里。切换
	// 渠道的用户应在凭据文件补上对应分组，而不是依赖回退。
	fallback := ZcodeChannelBigmodel
	if channel == fallback {
		fallback = ZcodeChannelZai
	}
	return entries[fallback]
}

// parseZcodeTokenStoreFile 解析一个凭据文件为 {渠道: key}。文件缺失或
// 损坏返回空表而非错误：凭据链还有其他环节，部分写入的文件不能拖垮请求。
// 兼容分组（{bigmodel: {...}}）与平铺（{provider, api_key, expires_at}）
// 两种形状；拒绝 eyJhbGci 开头的平台 JWT 与已过期的记录。
func parseZcodeTokenStoreFile(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var document map[string]any
	if json.Unmarshal(b, &document) != nil || document == nil {
		return nil
	}
	entries := map[string]string{}
	for _, provider := range []string{ZcodeChannelBigmodel, ZcodeChannelZai} {
		if key := zcodeKeyFromRecord(document[provider]); key != "" {
			entries[provider] = key
		}
	}
	// 旧平铺形状：{provider, api_key, expires_at}；分组键缺失时为空，直接跳过。
	if provider, _ := document["provider"].(string); provider != "" && entries[provider] == "" {
		if key := zcodeKeyFromRecord(document); key != "" {
			entries[provider] = key
		}
	}
	return entries
}

// zcodeKeyFromRecord 从一条凭据记录提取可用 key。provider 字段存在但不
// 认识时拒绝；api_key 优先于 access_token；带 expires_at 时过期即拒绝；
// eyJhbGci 开头的值是参考客户端曾误存的平台 JWT，降级为无凭据。
func zcodeKeyFromRecord(value any) string {
	record, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if provider, ok := record["provider"].(string); ok && provider != "" {
		if provider != ZcodeChannelBigmodel && provider != ZcodeChannelZai {
			return ""
		}
	}
	key := ""
	if v, ok := record["api_key"].(string); ok && v != "" {
		key = v
	} else if v, ok := record["access_token"].(string); ok && v != "" {
		key = v
	}
	if key == "" || strings.HasPrefix(key, "eyJhbGci") {
		return ""
	}
	if expires, ok := record["expires_at"].(float64); ok && expires > 0 {
		expiresMs := int64(expires)
		if expires < 1_000_000_000_000 {
			expiresMs *= 1000
		}
		if time.Now().UnixMilli() >= expiresMs {
			return ""
		}
	}
	return key
}
