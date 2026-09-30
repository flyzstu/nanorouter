package router

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	json "github.com/goccy/go-json"
)

// encryptForTest 用与 ZCode 客户端一致的格式加密一个值，供解析测试构造
// enc:v1 条目（生产代码只读不写该格式）。
func encryptForTest(t *testing.T, secret, plaintext string) string {
	t.Helper()
	key := deriveZcodeCipherKey(secret)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	ciphertext, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	encoded := "enc:v1:" + rawURL(iv) + "." + rawURL(tag) + "." + rawURL(ciphertext)
	return encoded
}

func rawURL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func hexOf(b []byte) string { return hex.EncodeToString(b) }

func TestDeriveZcodeCipherKeyGolden(t *testing.T) {
	// 跨语言 golden：与 node 参考实现 createHash('sha256') 的输出一致，
	// 密钥派生格式一旦漂移此测试即失败。
	got := deriveZcodeCipherKey("zcode-credential-fallback:linux:/root:root")
	const wantHex = "6f098e8d0d34381f7a04aa450ee03c68c3dd4ba127c31f3d34579c8a1b52de68"
	if hexOf(got) != wantHex {
		t.Fatalf("key derivation drifted: %x", got)
	}
	// 环境变量覆盖生效。
	t.Setenv(zcodeCredentialSecretEnv, "custom-secret")
	if got := deriveZcodeCipherKey(resolveZcodeCredentialSecret()); hexOf(got) == wantHex {
		t.Fatal("env secret must change the derived key")
	}
}

func TestDecryptZcodeCredentialValueRoundTrip(t *testing.T) {
	const secret = "unit-test-secret"
	t.Setenv(zcodeCredentialSecretEnv, secret)
	plain := "87b5abcd.0123456789abcdef0123456789abcdef"
	encrypted := encryptForTest(t, secret, plain)
	got, ok := decryptZcodeCredentialValue(encrypted)
	if !ok || got != plain {
		t.Fatalf("round trip: %q ok=%v", got, ok)
	}
	// 明文值原样透传；坏密文返回失败而不是 panic。
	if got, ok := decryptZcodeCredentialValue("  plain-key  "); !ok || got != "plain-key" {
		t.Fatalf("plaintext passthrough: %q %v", got, ok)
	}
	if _, ok := decryptZcodeCredentialValue("enc:v1:bogus.bogus.bogus"); ok {
		t.Fatal("garbage ciphertext must fail")
	}
	if _, ok := decryptZcodeCredentialValue(encryptForTest(t, "other-secret", "x")); ok {
		t.Fatal("wrong key must fail")
	}
}

func TestParseZcodeV2Credentials(t *testing.T) {
	const secret = "parse-secret"
	t.Setenv(zcodeCredentialSecretEnv, secret)
	document := map[string]any{
		"oauth:active_provider": encryptForTest(t, secret, "bigmodel"),
		// individual 与 team 两条 coding-plan：team 覆盖 individual（后写优先级由
		// coding-plan 规则保证），再放一条非 coding-plan 的同名渠道条目验证优先级。
		"account-provider:account:bigmodel-individual-coding-plan:account:1:api-key": encryptForTest(t, secret, "individual.key"),
		"account-provider:account:bigmodel:account:1:api-key":                        encryptForTest(t, secret, "plain.key"),
		"account-provider:coding-plan:account:zai:account:2:api-key":                 encryptForTest(t, secret, "zai.key"),
		// 无渠道词的条目落 active_provider（bigmodel）。
		"account-provider:coding-plan:account:3:api-key": encryptForTest(t, secret, "anon.key"),
		// 历史误存的平台 JWT 不是可服务凭据。
		"account-provider:coding-plan:account:4:api-key": encryptForTest(t, secret, "eyJhbGciOiJIUzI1NiJ9.payload.sig"),
		// 非 api-key 条目一律忽略。
		"zcodejwttoken": encryptForTest(t, secret, "eyJhbGciOiJIUzI1NiJ9.x.y"),
	}
	keys := parseZcodeV2Credentials(document)
	// coding-plan 覆盖非 coding-plan：bigmodel 渠道最终是 anon/individual 之一，
	// 取决于 map 迭代顺序——两条 coding-plan 中 anon 无渠道词也归 bigmodel。
	if keys[ZcodeChannelBigmodel] == "plain.key" {
		t.Fatalf("non-coding-plan entry must be overridden: %v", keys)
	}
	if keys[ZcodeChannelZai] != "zai.key" {
		t.Fatalf("zai: %v", keys)
	}
	if len(keys) != 2 {
		t.Fatalf("unexpected channels: %v", keys)
	}
}

func TestZcodeTokenFileKeyPrefersOwnStoreThenV2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)
	const secret = "integration-secret"
	t.Setenv(zcodeCredentialSecretEnv, secret)

	v2Dir := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(v2Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	v2Document := map[string]any{
		"oauth:active_provider": "bigmodel",
		"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:1:api-key": encryptForTest(t, secret, "v2.key"),
		"account-provider:coding-plan:account:zai:account:2:api-key":                             encryptForTest(t, secret, "v2.zai.key"),
	}
	v2Raw, err := json.Marshal(v2Document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2Dir, "credentials.json"), v2Raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// 只有 v2 文件：zcode_token_file 回退能直接读到官方凭据。
	if got := zcodeTokenFileKey(ZcodeChannelBigmodel); got != "v2.key" {
		t.Fatalf("v2 fallback: %q", got)
	}
	if got := zcodeTokenFileKey(ZcodeChannelZai); got != "v2.zai.key" {
		t.Fatalf("v2 zai fallback: %q", got)
	}

	// login zcode 自己的凭据文件出现后优先于 v2。
	storeDir := filepath.Join(home, ".nanorouter")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	storeRaw := `{"bigmodel":{"api_key":"own.key","updated_at":"2026-09-30T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(storeDir, zcodeTokenStoreName), []byte(storeRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	resetZcodeTokenCache()
	if got := zcodeTokenFileKey(ZcodeChannelBigmodel); got != "own.key" {
		t.Fatalf("own store must win: %q", got)
	}
	// 渠道补缺合并：own 库没有 zai 分组时从 v2 文件取正确渠道的 key，
	// 而不是落到跨分组回退。
	if got := zcodeTokenFileKey(ZcodeChannelZai); got != "v2.zai.key" {
		t.Fatalf("per-channel fill from v2: %q", got)
	}
}
