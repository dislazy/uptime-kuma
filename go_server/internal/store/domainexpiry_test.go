package store

import (
	"context"
	"testing"
	"time"
)

// TestDomainExpiryCRUD 覆盖 domain_expiry 的查、写、NULL 语义与更新。
func TestDomainExpiryCRUD(t *testing.T) {
	ctx := context.Background()
	ds := NewDomainExpiryStore(newTestDB(t))

	// 无记录 → nil
	if b, err := ds.FindByName(ctx, "example.com"); err != nil || b != nil {
		t.Fatalf("空库应返回 nil: %v %v", b, err)
	}

	// 插入（expiry 为 NULL，官方首查失败时的形状）
	b := &DomainExpiry{Domain: "example.com"}
	if err := ds.Save(ctx, b); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if b.ID <= 0 {
		t.Fatalf("自增 ID = %d", b.ID)
	}

	got, err := ds.FindByName(ctx, "example.com")
	if err != nil || got == nil {
		t.Fatalf("查询失败: %v %v", got, err)
	}
	if got.Expiry != nil || got.LastCheck != nil || got.LastExpiryNotificationSent != nil {
		t.Fatalf("NULL 语义应全部为 nil: %+v", got)
	}

	// 更新到期日与通知标记
	expiry := time.Date(2027, 5, 30, 4, 0, 0, 0, time.UTC)
	got.Expiry = &expiry
	now := time.Now().UTC().Truncate(time.Millisecond)
	got.LastCheck = &now
	sent := int64(7)
	got.LastExpiryNotificationSent = &sent
	if err := ds.Save(ctx, got); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	got2, err := ds.FindByName(ctx, "example.com")
	if err != nil || got2 == nil {
		t.Fatalf("二次查询失败: %v", err)
	}
	if got2.Expiry == nil || !got2.Expiry.Equal(expiry) {
		t.Fatalf("expiry 往返不一致: %v", got2.Expiry)
	}
	if got2.LastExpiryNotificationSent == nil || *got2.LastExpiryNotificationSent != 7 {
		t.Fatalf("lastSent 往返不一致: %v", got2.LastExpiryNotificationSent)
	}
}
