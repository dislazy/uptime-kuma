package store

import (
	"context"
	"testing"
)

// TestProxyCRUD 覆盖 proxy 的增改删查、协议校验、default 置换与 active 恒真。
func TestProxyCRUD(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMonitorStore(db)

	// 协议校验：官方 Unsupported proxy protocol
	if _, err := ms.SaveProxy(ctx, 0, "user-1", Proxy{Protocol: "grpc", Host: "127.0.0.1", Port: 8080}); err == nil {
		t.Fatal("非法协议未被拒绝")
	}

	// 新建
	id, err := ms.SaveProxy(ctx, 0, "user-1", Proxy{
		Protocol: "socks5", Host: "127.0.0.1", Port: 1080,
		Auth: true, Username: "u", Password: "p",
		Active: false, // 官方 `active || true` 恒真，false 也会被存成 1
	})
	if err != nil {
		t.Fatalf("新增失败: %v", err)
	}

	list, err := ms.Proxies(ctx)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("列表长度 = %d", len(list))
	}
	p := list[0]
	if p.Protocol != "socks5" || p.Host != "127.0.0.1" || p.Port != 1080 {
		t.Fatalf("内容不对: %+v", p)
	}
	if !p.Active {
		t.Fatal("active 应恒为 true（官方 `proxy.active || true` 恒真语义）")
	}
	if p.CreatedDate == "" {
		t.Fatal("created_date 应有默认值")
	}

	// ActiveProxyByID 命中
	got, err := ms.ActiveProxyByID(ctx, id)
	if err != nil {
		t.Fatalf("按 ID 读取失败: %v", err)
	}
	if got.Auth != true || got.Username != "u" {
		t.Fatalf("认证字段不对: %+v", got)
	}
	if got.ProxyURL() != "socks5://u:p@127.0.0.1:1080" {
		t.Fatalf("ProxyURL = %q", got.ProxyURL())
	}

	// 更新 + 设为 default
	if _, err := ms.SaveProxy(ctx, id, "user-1", Proxy{
		Protocol: "http", Host: "10.0.0.9", Port: 3128, Default: true,
	}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	// 第二个 default 应把第一个顶掉
	id2, err := ms.SaveProxy(ctx, 0, "user-1", Proxy{
		Protocol: "http", Host: "10.0.0.10", Port: 3128, Default: true,
	})
	if err != nil {
		t.Fatalf("新增第二个失败: %v", err)
	}
	list, _ = ms.Proxies(ctx)
	var defaults int
	for _, x := range list {
		if x.Default {
			defaults++
		}
	}
	if defaults != 1 || !list[1].Default || list[1].ID != id2 {
		t.Fatalf("default 置换不对: %+v", list)
	}

	// 更新不存在的 ID
	if _, err := ms.SaveProxy(ctx, 9999, "user-1", Proxy{Protocol: "http", Host: "h", Port: 1}); err == nil {
		t.Fatal("更新不存在的代理应报错")
	}
}

// TestProxyDeleteCascade 删除代理后 monitor.proxy_id 应被置空。
func TestProxyDeleteCascade(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMonitorStore(db)

	pid, err := ms.SaveProxy(ctx, 0, "user-1", Proxy{Protocol: "http", Host: "127.0.0.1", Port: 3128})
	if err != nil {
		t.Fatal(err)
	}
	mid, err := ms.Create(ctx, map[string]any{
		"name": "web", "type": "http", "url": "https://example.com", "proxy_id": pid,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := ms.DeleteProxy(ctx, pid); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := ms.ActiveProxyByID(ctx, pid); err == nil {
		t.Fatal("删除后仍能读到")
	}
	if err := ms.DeleteProxy(ctx, pid); err == nil {
		t.Fatal("重复删除应报 proxy not found")
	}

	m, err := ms.Get(ctx, mid)
	if err != nil {
		t.Fatal(err)
	}
	if v := m.Get("proxy_id"); v != "" && v != "0" {
		t.Fatalf("删除后 monitor.proxy_id 未置空: %q", v)
	}
}

// TestProxyApplyExisting 勾选 applyExisting 时所有监控的 proxy_id 应指向新代理。
func TestProxyApplyExisting(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMonitorStore(db)

	m1, _ := ms.Create(ctx, map[string]any{"name": "a", "type": "http", "url": "https://a.example"})
	m2, _ := ms.Create(ctx, map[string]any{"name": "b", "type": "http", "url": "https://b.example"})

	pid, err := ms.SaveProxy(ctx, 0, "user-1", Proxy{
		Protocol: "http", Host: "127.0.0.1", Port: 3128, ApplyExisting: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{m1, m2} {
		m, err := ms.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if m.GetInt("proxy_id") != pid {
			t.Fatalf("监控 %d 的 proxy_id = %d, 期望 %d", id, m.GetInt("proxy_id"), pid)
		}
	}
}
