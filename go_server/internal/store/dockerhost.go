package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/songbo/uptime-kuma/go_server/internal/checkers"
)

// DockerHost 对应官方 docker_host 表。
//
// 官方把 Docker 连接信息单独建表（一个用户可配多个 Docker 主机），
// monitor 表只存 docker_host 这个外键。这里保持同样结构，老库直接可用。
type DockerHost struct {
	ID           int64  `json:"id"`
	UserID       any    `json:"userId"`
	Name         string `json:"name"`
	DockerDaemon string `json:"dockerDaemon"`
	DockerType   string `json:"dockerType"`
}

// JSON 返回给前端的形态，字段名与官方 bean.toJSON() 一致（snake_case 列名）。
func (h DockerHost) JSON() map[string]any {
	return map[string]any{
		"id":            h.ID,
		"user_id":       h.UserID,
		"name":          h.Name,
		"docker_daemon": h.DockerDaemon,
		"docker_type":   h.DockerType,
	}
}

// DockerHosts 列出全部 Docker 主机。
func (s *MonitorStore) DockerHosts(ctx context.Context) ([]DockerHost, error) {
	if !s.db.HasTable(ctx, "docker_host") {
		return nil, nil
	}
	rows, err := s.db.Query(ctx,
		"SELECT id, user_id, name, docker_daemon, docker_type FROM docker_host ORDER BY id")
	if err != nil {
		return nil, err
	}

	out := make([]DockerHost, 0, len(rows))
	for _, r := range rows {
		out = append(out, DockerHost{
			ID:           r.Int("id"),
			UserID:       r["user_id"],
			Name:         r.Str("name"),
			DockerDaemon: r.Str("docker_daemon"),
			DockerType:   r.Str("docker_type"),
		})
	}
	return out, nil
}

// SaveDockerHost 新建或更新一台 Docker 主机，返回 ID。
func (s *MonitorStore) SaveDockerHost(
	ctx context.Context,
	id int64,
	userID string,
	name, daemon, typ string,
) (int64, error) {
	if !s.db.HasTable(ctx, "docker_host") {
		return 0, errors.New("docker_host table is missing")
	}

	values := map[string]any{
		"name":          name,
		"docker_daemon": daemon,
		"docker_type":   typ,
	}
	if userID != "" {
		values["user_id"] = userID
	}

	if id > 0 {
		found, err := s.db.Query(ctx, "SELECT id FROM docker_host WHERE id = ?", id)
		if err != nil {
			return 0, err
		}
		if len(found) == 0 {
			return 0, errors.New("docker host not found")
		}
		if err := s.db.Update(ctx, "docker_host", id, values); err != nil {
			return 0, err
		}
		return id, nil
	}

	return s.db.Insert(ctx, "docker_host", values)
}

// DeleteDockerHost 删除一台 Docker 主机，并把引用它的监控置空。
// 官方同样先 UPDATE monitor SET docker_host = null。
func (s *MonitorStore) DeleteDockerHost(ctx context.Context, id int64) error {
	if !s.db.HasTable(ctx, "docker_host") {
		return errors.New("docker_host table is missing")
	}
	found, err := s.db.Query(ctx, "SELECT id FROM docker_host WHERE id = ?", id)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return errors.New("docker host not found")
	}
	if _, err := s.db.Exec(ctx,
		"UPDATE monitor SET docker_host = NULL WHERE docker_host = ?", id); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, "DELETE FROM docker_host WHERE id = ?", id)
	return err
}

// DockerHostByID 按 ID 取连接信息，供 checker 使用。
func (s *MonitorStore) DockerHostByID(ctx context.Context, id int64) (checkers.DockerHostInfo, error) {
	if id <= 0 || !s.db.HasTable(ctx, "docker_host") {
		return checkers.DockerHostInfo{}, fmt.Errorf("Failed to load docker host config")
	}
	rows, err := s.db.Query(ctx,
		"SELECT docker_daemon, docker_type FROM docker_host WHERE id = ?", id)
	if err != nil {
		return checkers.DockerHostInfo{}, err
	}
	if len(rows) == 0 {
		return checkers.DockerHostInfo{}, fmt.Errorf("Failed to load docker host config")
	}
	return checkers.DockerHostInfo{
		Daemon: rows[0].Str("docker_daemon"),
		Type:   rows[0].Str("docker_type"),
	}, nil
}
