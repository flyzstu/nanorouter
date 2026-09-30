package router

// ZCode 桌面/CLI 的官方凭据文件（~/.zcode/v2/credentials.json）读取支持。
//
// 该文件由 ZCode 客户端以 AES-256-GCM 加密落盘，值为
// `enc:v1:<base64url_iv>.<base64url_auth_tag>.<base64url_ciphertext>`；
// 密钥 = SHA-256(`ZCODE_CREDENTIAL_SECRET` 环境变量)，未设置时回退
// `zcode-credential-fallback:{platform}:{homedir}:{username}`。常量、格式
// 与字段选择规则 1:1 对齐 dsh-zhipu-provider 的 src/cipher.ts / auth.ts
// （后者对齐 ZCode 客户端本体）。这里只读不写：文件归 ZCode 客户端所有，
// 并发登录会竞态。
//
// 解析目标只是"渠道 → 可服务 key"：api-key 后缀条目按 key 名里的渠道词
// 归类（含 bigmodel/zai，否则落在 active_provider，再退 bigmodel），
// coding-plan 条目优先于同渠道的非 coding-plan 条目；`eyJhbGci` 平台 JWT
// 视为无凭据（与参考客户端历史误存行为的兼容一致）。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	json "github.com/goccy/go-json"
)

const zcodeEncryptedValuePrefix = "enc:v1:"
const zcodeCredentialSecretEnv = "ZCODE_CREDENTIAL_SECRET"
const zcodeDataBaseDirEnv = "ZCODE_DATA_BASE_DIR"

// ZcodeV2CredentialsPath 返回 ZCode 官方凭据文件路径（只读）。
// ZCODE_DATA_BASE_DIR 重定向数据目录时跟随；否则固定 ~/.zcode/v2/。
func ZcodeV2CredentialsPath() string {
	if base := strings.TrimSpace(os.Getenv(zcodeDataBaseDirEnv)); base != "" {
		return filepath.Join(base, "v2", "credentials.json")
	}
	home := zcodeHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".zcode", "v2", "credentials.json")
}

// resolveZcodeCredentialSecret 解出凭据加密密钥的种子串：环境变量优先，
// 未设置时用平台/家目录/用户名拼回退串（与 ZCode 客户端的默认推导一致，
// 同机同用户必然同串）。
func resolveZcodeCredentialSecret() string {
	if secret := strings.TrimSpace(os.Getenv(zcodeCredentialSecretEnv)); secret != "" {
		return secret
	}
	home, _ := os.UserHomeDir()
	username := "unknown"
	if current, err := user.Current(); err == nil {
		username = current.Username
	}
	return fmt.Sprintf("zcode-credential-fallback:%s:%s:%s", zcodePlatformName(), home, username)
}

// deriveZcodeCipherKey 由种子串派生 AES-256 密钥。
func deriveZcodeCipherKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// decryptZcodeCredentialValue 解一个凭据值：非加密值原样返回，解密失败
// 返回 false（调用方跳过该条目，坏一条目不能拖垮整份文件）。
func decryptZcodeCredentialValue(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, zcodeEncryptedValuePrefix) {
		return value, true
	}
	parts := strings.Split(value[len(zcodeEncryptedValuePrefix):], ".")
	if len(parts) != 3 {
		return "", false
	}
	iv, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	authTag, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	ciphertext, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(iv) != 12 || len(authTag) != 16 {
		return "", false
	}
	block, err := aes.NewCipher(deriveZcodeCipherKey(resolveZcodeCredentialSecret()))
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	// Node 侧 auth tag 独立存放；Go 的 GCM.Open 期望 ciphertext||tag。
	plaintext, err := gcm.Open(nil, iv, append(ciphertext, authTag...), nil)
	if err != nil {
		return "", false
	}
	return string(plaintext), true
}

// parseZcodeV2Credentials 把解密后的 v2 凭据文档归并为 {渠道: 可服务 key}。
func parseZcodeV2Credentials(document map[string]any) map[string]string {
	entries := map[string]string{}
	if document == nil {
		return entries
	}
	activeProvider := ""
	if raw, ok := document["oauth:active_provider"]; ok {
		if decoded, ok := decryptZcodeCredentialValue(toString(raw)); ok && decoded != "" {
			activeProvider = strings.ToLower(decoded)
		}
	}
	for key, raw := range document {
		lower := strings.ToLower(key)
		if !isZcodeV2APIKeyField(lower) {
			continue
		}
		decoded, ok := decryptZcodeCredentialValue(toString(raw))
		if !ok || decoded == "" || strings.HasPrefix(decoded, "eyJhbGci") {
			continue
		}
		provider := activeProvider
		switch {
		case strings.Contains(lower, "bigmodel"):
			provider = ZcodeChannelBigmodel
		case strings.Contains(lower, "zai"):
			provider = ZcodeChannelZai
		case provider == "":
			provider = ZcodeChannelBigmodel
		}
		// coding-plan 条目优先：同渠道已有非 coding-plan 值时仍被覆盖。
		if _, exists := entries[provider]; exists && !strings.Contains(lower, "coding-plan") {
			continue
		}
		entries[provider] = decoded
	}
	return entries
}

func isZcodeV2APIKeyField(lower string) bool {
	return strings.HasSuffix(lower, ":api-key") || strings.HasSuffix(lower, ":apikey") ||
		strings.HasSuffix(lower, ":api_key") || lower == "api-key" || lower == "apikey" || lower == "api_key"
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// parseZcodeV2CredentialsFile 读文件并解析为 {渠道: key}；文件缺失、损坏
// 或整体解密失败返回空表——凭据链还有其他来源，坏文件不能拖垮请求。
func parseZcodeV2CredentialsFile(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return nil
	}
	return parseZcodeV2Credentials(document)
}
