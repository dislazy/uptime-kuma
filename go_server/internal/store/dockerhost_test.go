package store

import (
	"context"
	"path/filepath"
	"testing"
)

// newTestDB 建一个带完整 schema 的临时库。
func newTestDB(t *testing.T) *DB {
	t.Helper()

	db, err := Open(filepath.Join(t.TempDir(), "kuma.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("初始化 schema 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestDockerHostCRUD 覆盖 docker_host 的增改删查与解引用。
func TestDockerHostCRUD(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMonitorStore(db)

	id, err := ms.SaveDockerHost(ctx, 0, "user-1", "local", "/var/run/docker.sock", "socket")
	if err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("ID = %d", id)
	}

	list, err := ms.DockerHosts(ctx)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(list) != 1 || list[0].DockerDaemon != "/var/run/docker.sock" || list[0].DockerType != "socket" {
		t.Fatalf("列表内容不对: %+v", list)
	}

	// 更新
	if _, err := ms.SaveDockerHost(ctx, id, "user-1", "remote", "tcp://10.0.0.5:2375", "tcp"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	info, err := ms.DockerHostByID(ctx, id)
	if err != nil {
		t.Fatalf("按 ID 读取失败: %v", err)
	}
	if info.Daemon != "tcp://10.0.0.5:2375" || info.Type != "tcp" {
		t.Fatalf("更新后内容不对: %+v", info)
	}

	// 建一个引用它的监控，删除后应被置空
	mid, err := ms.Create(ctx, map[string]any{
		"name":             "web",
		"type":             "docker",
		"docker_host":      id,
		"docker_container": "web",
	})
	if err != nil {
		t.Fatalf("建监控失败: %v", err)
	}

	if err := ms.DeleteDockerHost(ctx, id); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	if _, err := ms.DockerHostByID(ctx, id); err == nil {
		t.Fatal("删除后仍能按 ID 读到")
	}

	m, err := ms.Get(ctx, mid)
	if err != nil {
		t.Fatalf("读监控失败: %v", err)
	}
	if v := m.Get("docker_host"); v != "" && v != "0" {
		t.Fatalf("删除后 monitor.docker_host 未置空: %q", v)
	}
}

// TestDockerHostHydrate 校验 docker 监控在加载时能拿到解析后的连接信息。
func TestDockerHostHydrate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMonitorStore(db)

	hid, err := ms.SaveDockerHost(ctx, 0, "user-1", "local", "tcp://127.0.0.1:2375", "tcp")
	if err != nil {
		t.Fatal(err)
	}

	mid, err := ms.Create(ctx, map[string]any{
		"name":             "web",
		"type":             "docker",
		"docker_host":      hid,
		"docker_container": "web",
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := ms.Get(ctx, mid)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get("_docker_daemon"); got != "tcp://127.0.0.1:2375" {
		t.Fatalf("_docker_daemon = %q", got)
	}
	if got := m.Get("_docker_type"); got != "tcp" {
		t.Fatalf("_docker_type = %q", got)
	}
}
