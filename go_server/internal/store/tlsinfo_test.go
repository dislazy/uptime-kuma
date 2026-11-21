package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestTLSInfoUpsertAndCertChange 覆盖官方 updateTlsInfo 语义：
// 首次插入、二次更新、证书变化时清空 certificate 通知历史。
func TestTLSInfoUpsertAndCertChange(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "kuma.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("初始化 schema 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := NewTLSInfoStore(db)

	// 官方形状的 info_json（只需 fingerprint256 参与变更检测）
	certA := `{"certInfo":{"fingerprint256":"AA:01"},"valid":true}`
	certB := `{"certInfo":{"fingerprint256":"BB:02"},"valid":true}`
	certA2 := `{"certInfo":{"fingerprint256":"AA:01"},"valid":false}`

	// 官方 schema：monitor.user_id 引用 user 表，先造真实外键
	if _, err := db.Exec(ctx, `INSERT INTO [user] (id, username, password) VALUES (1, 'admin', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO monitor (id, user_id, name, type, interval, active) VALUES (1, 1, 'm', 'http', 60, 1)`); err != nil {
		t.Fatal(err)
	}

	// 首次插入
	if _, err := s.Upsert(ctx, 1, certA); err != nil {
		t.Fatalf("首次 upsert: %v", err)
	}
	got, ok, _ := s.InfoJSON(ctx, 1)
	if !ok || got != certA {
		t.Fatalf("InfoJSON = %q ok=%v", got, ok)
	}

	// 预置一条证书通知历史
	if err := s.MarkCertNotification(ctx, 1, 14); err != nil {
		t.Fatal(err)
	}

	// 同证书更新：不应清历史
	changed, err := s.Upsert(ctx, 1, certA2)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("指纹未变不应报 certChanged")
	}
	if sent, _ := s.HasCertNotification(ctx, 1, 14); !sent {
		t.Fatal("指纹未变时通知历史应保留")
	}

	// 换证书：应清历史
	changed, err = s.Upsert(ctx, 1, certB)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("指纹变化应报 certChanged")
	}
	if sent, _ := s.HasCertNotification(ctx, 1, 14); sent {
		t.Fatal("换证书后通知历史应被清空（官方语义）")
	}

	// 去重语义：days <= target 命中
	if err := s.MarkCertNotification(ctx, 1, 14); err != nil {
		t.Fatal(err)
	}
	if sent, _ := s.HasCertNotification(ctx, 1, 21); !sent {
		t.Fatal("已发过 14 天档，21 天档应视为已发过（days <= target）")
	}
	if sent, _ := s.HasCertNotification(ctx, 1, 7); sent {
		t.Fatal("已发过 14 天档，7 天档未发过，应可再发")
	}
}
