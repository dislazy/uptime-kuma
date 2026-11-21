package domainexpiry

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// newDomainExpiryTestStore 建一个带完整 schema 的临时库。
func newDomainExpiryTestStore(t *testing.T) *store.DomainExpiryStore {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kuma.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("初始化 schema 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store.NewDomainExpiryStore(db)
}

// newStubService 用内嵌数据 + 可控 fetch 构造 Service。
func newStubService(t *testing.T) *Service {
	t.Helper()
	svc := NewService(nil)
	// 拉取失败 → 走内嵌兜底数据（离线可用语义）
	svc.fetch = func(ctx context.Context, url string) ([]byte, error) {
		if strings.Contains(url, "data.iana.org") {
			return nil, context.DeadlineExceeded
		}
		return nil, context.DeadlineExceeded
	}
	return svc
}

// TestCheckSupportBranches 覆盖官方 checkSupport 四层校验 + 成功路径。
func TestCheckSupportBranches(t *testing.T) {
	ctx := context.Background()
	svc := newStubService(t)

	cases := []struct {
		name       string
		typ, url   string
		hostname   string
		grpcURL    string
		wantErr    string
		wantMeta   map[string]string
		wantDomain string
		wantTld    string
	}{
		{
			name: "不支持的监控类型", typ: "docker",
			wantErr: ErrUnsupportedMonitorType,
		},
		{
			name: "目标为空", typ: "http",
			wantErr: ErrMissingTarget,
		},
		{
			name: "IP 非 ICANN", typ: "http", url: "https://192.168.1.1",
			wantErr: ErrNotICANN, wantMeta: map[string]string{"domain": "192.168.1.1", "publicSuffix": ""},
		},
		{
			name: "PSL 私有段非 ICANN", typ: "http", url: "https://foo.github.io",
			wantErr: ErrNotICANN, wantMeta: map[string]string{"domain": "foo.github.io", "publicSuffix": "github.io"},
		},
		{
			name: "hostname 字段类型", typ: "port", hostname: "example.com:443",
			wantDomain: "example.com", wantTld: "com",
		},
		{
			name: "grpcUrl 字段类型", typ: "grpc-keyword", grpcURL: "example.com:12345",
			wantDomain: "example.com", wantTld: "com",
		},
		{
			name: "完整 URL", typ: "http", url: "https://www.example.com/some/path?q=1",
			wantDomain: "example.com", wantTld: "com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			domain, tld, serr := svc.CheckSupport(ctx, tc.typ, tc.url, tc.hostname, tc.grpcURL)
			if tc.wantErr != "" {
				if serr == nil || serr.Msg != tc.wantErr {
					t.Fatalf("want err %s, got %+v", tc.wantErr, serr)
				}
				for k, v := range tc.wantMeta {
					if got := serr.Meta[k]; got != v {
						t.Fatalf("meta[%s] = %v, want %v", k, got, v)
					}
				}
				return
			}
			if serr != nil {
				t.Fatalf("unexpected err: %+v", serr)
			}
			if domain != tc.wantDomain || tld != tc.wantTld {
				t.Fatalf("domain/tld = %q/%q, want %q/%q", domain, tld, tc.wantDomain, tc.wantTld)
			}
		})
	}
}

// TestCheckExpiryCacheAndReset 覆盖一天缓存与到期日变远重置通知标记。
func TestCheckExpiryCacheAndReset(t *testing.T) {
	ctx := context.Background()
	svc := newStubService(t)

	future := time.Now().UTC().Add(100 * 24 * time.Hour).Truncate(time.Second)
	far := future.Add(50 * 24 * time.Hour)
	rdapBody, _ := json.Marshal(map[string]any{
		"events": []map[string]any{
			{"eventAction": "registration", "eventDate": "2020-01-01T00:00:00Z"},
			{"eventAction": "expiration", "eventDate": future.Format(time.RFC3339)},
		},
	})

	callCount := 0
	svc.fetch = func(c context.Context, u string) ([]byte, error) {
		if strings.Contains(u, "data.iana.org") {
			return nil, context.DeadlineExceeded // 内嵌数据兜底
		}
		callCount++
		if strings.Contains(u, "/domain/example.com") {
			return rdapBody, nil
		}
		return nil, context.DeadlineExceeded
	}

	db := newDomainExpiryTestStore(t)
	expiry := svc.CheckExpiry(ctx, db, "example.com")
	if expiry == nil || !expiry.Equal(future) {
		t.Fatalf("首次查询应得到到期日: %v", expiry)
	}

	// 二次查询走一天缓存：不再打 RDAP
	callCount = 0
	expiry2 := svc.CheckExpiry(ctx, db, "example.com")
	if callCount != 0 {
		t.Fatalf("一天内不应重查 RDAP，实际请求 %d 次", callCount)
	}
	if expiry2 == nil || !expiry2.Equal(future) {
		t.Fatalf("缓存值不一致: %v", expiry2)
	}

	// 手动把 lastCheck 改到一天前 → 重查；新到期日变远 → 通知标记重置
	if b, _ := db.FindByName(ctx, "example.com"); b != nil {
		old := time.Now().UTC().Add(-25 * time.Hour)
		b.LastCheck = &old
		sent := int64(7)
		b.LastExpiryNotificationSent = &sent
		if err := db.Save(ctx, b); err != nil {
			t.Fatalf("改 lastCheck 失败: %v", err)
		}
	}
	rdapBody2, _ := json.Marshal(map[string]any{
		"events": []map[string]any{{"eventAction": "expiration", "eventDate": far.Format(time.RFC3339)}},
	})
	svc.fetch = func(c context.Context, u string) ([]byte, error) {
		if strings.Contains(u, "/domain/example.com") {
			return rdapBody2, nil
		}
		return nil, context.DeadlineExceeded
	}
	expiry3 := svc.CheckExpiry(ctx, db, "example.com")
	if expiry3 == nil || !expiry3.Equal(far) {
		t.Fatalf("重查应得到新到期日: %v", expiry3)
	}
	if b, _ := db.FindByName(ctx, "example.com"); b == nil || b.LastExpiryNotificationSent != nil {
		t.Fatalf("到期日变远应重置通知标记: %+v", b)
	}
}

// TestServerForEmbedded 内嵌数据兜底：常见 TLD 能找到端点，未知 TLD 返回空。
func TestServerForEmbedded(t *testing.T) {
	ctx := context.Background()
	svc := newStubService(t)

	// 注：cn 未向 IANA 提交 RDAP（官方同样报 no-rdap），不在断言里
	for _, suffix := range []string{"com", "net", "org", "co.uk"} {
		endpoint, err := svc.serverFor(ctx, suffix)
		if err != nil {
			t.Fatalf("内嵌数据查询 %s 出错: %v", suffix, err)
		}
		if endpoint == "" {
			t.Fatalf("内嵌数据应有 %s 的 RDAP 端点", suffix)
		}
	}
	if endpoint, _ := svc.serverFor(ctx, "zzz-not-a-tld"); endpoint != "" {
		t.Fatalf("未知 TLD 应返回空: %q", endpoint)
	}
}

// TestDaysRemaining 对齐 dayjs 截断：30 天减一纳秒应为 29。
func TestDaysRemaining(t *testing.T) {
	expiry := time.Now().Add(30*24*time.Hour - time.Nanosecond)
	if got := DaysRemaining(expiry); got != 29 {
		t.Fatalf("DaysRemaining = %d, want 29", got)
	}
}
