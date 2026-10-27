package router

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestZcodeCredentialStoreRoundTrip(t *testing.T) {
	db := mustOpenMemoryDB(t)
	store := newZcodeCredentialStore(db)
	ctx := t.Context()

	if _, ok := store.get(ctx, ZcodeChannelBigmodel); ok {
		t.Fatal("空表不应有凭据")
	}
	want := ZcodeCredential{
		Channel: ZcodeChannelBigmodel, APIKey: "k1", PlatformJWT: "jwt",
		OAuthToken: "oauth", UserName: "张三", DeviceMid: "dev-1",
	}
	if err := store.save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok := store.get(ctx, ZcodeChannelBigmodel)
	if !ok || got.APIKey != "k1" || got.PlatformJWT != "jwt" || got.UserName != "张三" || got.DeviceMid != "dev-1" {
		t.Fatalf("读回=%+v ok=%v", got, ok)
	}
	if got.LastVerified.IsZero() {
		t.Fatal("登录成功即视为已验证，last_verified 不应为零")
	}

	// 同一渠道重复 save 应更新而非插入第二行。
	want.APIKey = "k2"
	if err := store.save(ctx, want); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM zcode_credentials WHERE channel = ?`, ZcodeChannelBigmodel).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("同一渠道应只有一行，实际 %d 行", rows)
	}
	got, _ = store.get(ctx, ZcodeChannelBigmodel)
	if got.APIKey != "k2" {
		t.Fatalf("重复保存未更新: %q", got.APIKey)
	}
}

func TestZcodeCredentialStoreCrossChannelFallback(t *testing.T) {
	db := mustOpenMemoryDB(t)
	store := newZcodeCredentialStore(db)
	ctx := t.Context()
	if err := store.save(ctx, ZcodeCredential{Channel: ZcodeChannelBigmodel, APIKey: "cn"}); err != nil {
		t.Fatal(err)
	}
	// 请求 zai 但只有 bigmodel 记录：跨渠道回退，与文件源语义一致。
	got, ok := store.get(ctx, ZcodeChannelZai)
	if !ok || got.APIKey != "cn" {
		t.Fatalf("跨渠道回退失败: %+v ok=%v", got, ok)
	}
}

func TestZcodeCredentialStoreRejectsIncomplete(t *testing.T) {
	db := mustOpenMemoryDB(t)
	store := newZcodeCredentialStore(db)
	if err := store.save(t.Context(), ZcodeCredential{APIKey: "k"}); err == nil {
		t.Fatal("缺 channel 应报错")
	}
	if err := store.save(t.Context(), ZcodeCredential{Channel: ZcodeChannelBigmodel}); err == nil {
		t.Fatal("缺 api_key 应报错")
	}
}

func TestZcodeCredentialStoreCacheInvalidatedBySave(t *testing.T) {
	db := mustOpenMemoryDB(t)
	store := newZcodeCredentialStore(db)
	ctx := t.Context()

	// 先查一次填充缓存（空结果也应被缓存）。
	if _, ok := store.get(ctx, ZcodeChannelBigmodel); ok {
		t.Fatal("不应有凭据")
	}
	if err := store.save(ctx, ZcodeCredential{Channel: ZcodeChannelBigmodel, APIKey: "fresh"}); err != nil {
		t.Fatal(err)
	}
	// save 会 invalidate，故不必等 5 秒 TTL。
	got, ok := store.get(ctx, ZcodeChannelBigmodel)
	if !ok || got.APIKey != "fresh" {
		t.Fatalf("写库后应立刻可见: %+v ok=%v", got, ok)
	}
}

func TestZcodeCredentialStoreConcurrentLoad(t *testing.T) {
	db := mustOpenMemoryDB(t)
	store := newZcodeCredentialStore(db)
	ctx := t.Context()
	if err := store.save(ctx, ZcodeCredential{Channel: ZcodeChannelBigmodel, APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	store.invalidate()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, ok := store.get(ctx, ZcodeChannelBigmodel)
			if !ok || got.APIKey != "k" {
				errs <- fmt.Errorf("get = %+v, %v", got, ok)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestZcodeCredentialStoreDeviceMid(t *testing.T) {
	db := mustOpenMemoryDB(t)
	store := newZcodeCredentialStore(db)
	ctx := t.Context()
	if store.deviceMid(ctx) != "" {
		t.Fatal("空表不应有设备身份")
	}
	if err := store.save(ctx, ZcodeCredential{Channel: ZcodeChannelZai, APIKey: "k", DeviceMid: "dev-9"}); err != nil {
		t.Fatal(err)
	}
	if got := store.deviceMid(ctx); got != "dev-9" {
		t.Fatalf("device_mid=%q", got)
	}
}

func TestZcodeCredentialStoreCacheHoldsWithinTTL(t *testing.T) {
	db := mustOpenMemoryDB(t)
	store := newZcodeCredentialStore(db)
	ctx := t.Context()
	if err := store.save(ctx, ZcodeCredential{Channel: ZcodeChannelBigmodel, APIKey: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.get(ctx, ZcodeChannelBigmodel); !ok {
		t.Fatal("应读到凭据")
	}
	// 绕过 store 直接改库：缓存期内应读不到改动。
	if _, err := db.ExecContext(ctx, `UPDATE zcode_credentials SET api_key = 'bypass' WHERE channel = ?`, ZcodeChannelBigmodel); err != nil {
		t.Fatal(err)
	}
	got, _ := store.get(ctx, ZcodeChannelBigmodel)
	if got.APIKey != "first" {
		t.Fatalf("TTL 内应命中缓存，得到 %q", got.APIKey)
	}
	// 过期后重查。
	store.mu.Lock()
	store.at = time.Now().Add(-2 * zcodeCredentialCacheTTL)
	store.mu.Unlock()
	got, _ = store.get(ctx, ZcodeChannelBigmodel)
	if got.APIKey != "bypass" {
		t.Fatalf("TTL 过期后应重查，得到 %q", got.APIKey)
	}
}

func TestZcodeCredentialChainPrefersDatabaseOverFile(t *testing.T) {
	db := mustOpenMemoryDB(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetZcodeTokenCache()
	t.Cleanup(resetZcodeTokenCache)

	// 文件里放一个不同的 key，库里放另一个：库里优先。
	dir := home + "/.nanorouter"
	if err := writeTestFile(dir+"/"+zcodeTokenStoreName, `{"bigmodel":{"api_key":"file-key"}}`); err != nil {
		t.Fatal(err)
	}
	r := &Router{zcodeCredentials: newZcodeCredentialStore(db)}
	ctx := t.Context()
	if err := r.zcodeCredentials.save(ctx, ZcodeCredential{Channel: ZcodeChannelBigmodel, APIKey: "db-key"}); err != nil {
		t.Fatal(err)
	}
	got := r.resolveCandidateAPIKey(ctx, Candidate{ZcodeTokenFile: true, ZcodeChannel: ZcodeChannelBigmodel})
	if got != "db-key" {
		t.Fatalf("库应优先于文件，得到 %q", got)
	}

	// 环境变量仍高于库。
	t.Setenv("TEST_ZCODE_ENV", "env-key")
	got = r.resolveCandidateAPIKey(ctx, Candidate{
		APIKeyEnv: "TEST_ZCODE_ENV", ZcodeTokenFile: true, ZcodeChannel: ZcodeChannelBigmodel,
	})
	if got != "env-key" {
		t.Fatalf("环境变量应优先，得到 %q", got)
	}

	// 库里没有该渠道时回退文件。
	if err := r.zcodeCredentials.save(ctx, ZcodeCredential{Channel: ZcodeChannelZai, APIKey: "zai-key"}); err != nil {
		t.Fatal(err)
	}
	_ = writeTestFile(dir+"/"+zcodeTokenStoreName, `{"zai":{"api_key":"zai-file"}}`)
	resetZcodeTokenCache()
	r.zcodeCredentials.invalidate()
	got = r.resolveCandidateAPIKey(ctx, Candidate{ZcodeTokenFile: true, ZcodeChannel: ZcodeChannelZai})
	if got != "zai-key" {
		t.Fatalf("zai 应取库里的值，得到 %q", got)
	}
}

// writeTestFile 写一个文件并建好父目录，仅测试用。
func writeTestFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}
