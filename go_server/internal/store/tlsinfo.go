package store

import (
	"context"
	"encoding/json"
)

// TLSInfoStore 负责 monitor_tls_info 与证书通知去重历史
// （notification_sent_history 的 type='certificate' 部分）。
//
// 对齐官方 Monitor.updateTlsInfo（server/model/monitor.js）：
//   - 无记录 → 插入；有记录 → 更新
//   - 证书变化（fingerprint256 不同）→ 清空该监控的证书通知历史
type TLSInfoStore struct {
	db *DB
}

// NewTLSInfoStore 构造。
func NewTLSInfoStore(db *DB) *TLSInfoStore {
	return &TLSInfoStore{db: db}
}

// Upsert 写入 info_json。返回证书是否发生变化（用于调用方记录日志）。
func (s *TLSInfoStore) Upsert(ctx context.Context, monitorID int64, infoJSON string) (certChanged bool, err error) {
	oldJSON, exists, err := s.InfoJSON(ctx, monitorID)
	if err != nil {
		return false, err
	}

	if exists {
		if fingerprint256(oldJSON) != fingerprint256(infoJSON) {
			// 官方语义：换证书后重置已发送历史，让新证书的到期提醒能再次发出
			if _, err := s.db.Exec(ctx,
				"DELETE FROM notification_sent_history WHERE type = 'certificate' AND monitor_id = ?",
				monitorID); err != nil {
				return false, err
			}
			certChanged = true
		}
		_, err = s.db.Exec(ctx,
			"UPDATE monitor_tls_info SET info_json = ? WHERE monitor_id = ?", infoJSON, monitorID)
		return certChanged, err
	}

	_, err = s.db.Exec(ctx,
		"INSERT INTO monitor_tls_info (monitor_id, info_json) VALUES (?, ?)", monitorID, infoJSON)
	return certChanged, err
}

// InfoJSON 读取指定监控的 info_json。
func (s *TLSInfoStore) InfoJSON(ctx context.Context, monitorID int64) (string, bool, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT info_json FROM monitor_tls_info WHERE monitor_id = ?", monitorID)
	if err != nil || row == nil {
		return "", false, err
	}
	return row.Str("info_json"), true, nil
}

// HasCertNotification 判断是否已为某个目标天数发送过到期提醒
// （官方语义：days <= targetDays 命中即视为已发过）。
func (s *TLSInfoStore) HasCertNotification(ctx context.Context, monitorID int64, targetDays int64) (bool, error) {
	row, err := s.db.QueryOne(ctx,
		"SELECT id FROM notification_sent_history WHERE type = 'certificate' AND monitor_id = ? AND days <= ? LIMIT 1",
		monitorID, targetDays)
	if err != nil || row == nil {
		return false, err
	}
	return true, nil
}

// MarkCertNotification 记录一次成功发送的到期提醒。
func (s *TLSInfoStore) MarkCertNotification(ctx context.Context, monitorID int64, targetDays int64) error {
	_, err := s.db.Exec(ctx,
		"INSERT INTO notification_sent_history (type, monitor_id, days) VALUES ('certificate', ?, ?)",
		monitorID, targetDays)
	return err
}

// fingerprint256 从 info_json 里取叶子证书的 fingerprint256，取不到返回空串。
func fingerprint256(infoJSON string) string {
	var info struct {
		CertInfo struct {
			Fingerprint256 string `json:"fingerprint256"`
		} `json:"certInfo"`
	}
	if err := json.Unmarshal([]byte(infoJSON), &info); err != nil {
		return ""
	}
	return info.CertInfo.Fingerprint256
}
