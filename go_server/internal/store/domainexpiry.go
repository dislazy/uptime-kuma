package store

import (
	"context"
	"time"
)

// DomainExpiry 对齐官方 domain_expiry 表（domain_expiry.js BeanModel）。
type DomainExpiry struct {
	ID                         int64
	Domain                     string
	LastCheck                  *time.Time
	Expiry                     *time.Time
	LastExpiryNotificationSent *int64
}

// DomainExpiryStore 负责 domain_expiry 表。
type DomainExpiryStore struct {
	db *DB
}

// NewDomainExpiryStore 构造。
func NewDomainExpiryStore(db *DB) *DomainExpiryStore {
	return &DomainExpiryStore{db: db}
}

// FindByName 官方 DomainExpiry.findByName；无记录返回 nil。
func (s *DomainExpiryStore) FindByName(ctx context.Context, domain string) (*DomainExpiry, error) {
	row, err := s.db.QueryOne(ctx, "SELECT * FROM [domain_expiry] WHERE domain = ?", domain)
	if err != nil || row == nil {
		return nil, err
	}
	return hydrateDomainExpiry(row), nil
}

// Save 插入或更新（ID 为 0 走插入）。
func (s *DomainExpiryStore) Save(ctx context.Context, b *DomainExpiry) error {
	if b.ID > 0 {
		_, err := s.db.Exec(ctx,
			"UPDATE [domain_expiry] SET domain = ?, last_check = ?, expiry = ?, last_expiry_notification_sent = ? WHERE id = ?",
			b.Domain, timePtr(b.LastCheck), timePtr(b.Expiry), intPtr(b.LastExpiryNotificationSent), b.ID)
		return err
	}
	res, err := s.db.Exec(ctx,
		"INSERT INTO [domain_expiry] (domain, last_check, expiry, last_expiry_notification_sent) VALUES (?, ?, ?, ?)",
		b.Domain, timePtr(b.LastCheck), timePtr(b.Expiry), intPtr(b.LastExpiryNotificationSent))
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	return nil
}

// hydrateDomainExpiry 从通用行还原；NULL 列为 nil。
func hydrateDomainExpiry(row Row) *DomainExpiry {
	b := &DomainExpiry{
		ID:     row.Int("id"),
		Domain: row.Str("domain"),
	}
	if s := row.Str("last_check"); s != "" {
		if t, err := ParseTime(s); err == nil {
			b.LastCheck = &t
		}
	}
	if s := row.Str("expiry"); s != "" {
		if t, err := ParseTime(s); err == nil {
			b.Expiry = &t
		}
	}
	// last_expiry_notification_sent 存目标天数（7/14/21），0 视为未发送
	if n := row.Int("last_expiry_notification_sent"); n != 0 {
		b.LastExpiryNotificationSent = &n
	}
	return b
}

// timePtr 时间指针转官方格式字符串（nil → NULL）。
func timePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}

// intPtr 整数指针转驱动值（nil → NULL）。
func intPtr(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}
