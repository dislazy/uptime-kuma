package checkers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// dockerFakeDaemon 起一个假的 Docker Engine API，按 container 状态返回 JSON。
func dockerFakeDaemon(t *testing.T, state map[string]any) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "abc", "ImageID": "sha256:deadbeef"},
				{"Id": "def", "ImageID": "sha256:cafe"},
			})
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"State": state})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dockerMonitor(daemon, container string) *model.Monitor {
	return &model.Monitor{
		Type:     "docker",
		Interval: 20,
		Raw: map[string]any{
			"docker_container": container,
			RawDockerDaemon:    daemon,
			RawDockerType:      "tcp",
		},
	}
}

func TestDockerHealthy(t *testing.T) {
	srv := dockerFakeDaemon(t, map[string]any{
		"Status":  "running",
		"Running": true,
		"Health":  map[string]any{"Status": "healthy"},
	})

	res := DockerChecker{}.Check(context.Background(), dockerMonitor(srv.URL, "web"))
	if res.Status != model.StatusUp || res.Msg != "healthy" {
		t.Fatalf("得到 %d / %q，期望 UP / healthy", res.Status, res.Msg)
	}
}

func TestDockerRunningWithoutHealth(t *testing.T) {
	srv := dockerFakeDaemon(t, map[string]any{"Status": "running", "Running": true})

	res := DockerChecker{}.Check(context.Background(), dockerMonitor(srv.URL, "web"))
	if res.Status != model.StatusUp {
		t.Fatalf("状态 = %d，期望 UP（消息 %q）", res.Status, res.Msg)
	}
	if !strings.Contains(res.Msg, "currently running") {
		t.Fatalf("消息 = %q，应包含 currently running", res.Msg)
	}
}

func TestDockerUnhealthy(t *testing.T) {
	srv := dockerFakeDaemon(t, map[string]any{
		"Status":  "running",
		"Running": true,
		"Health":  map[string]any{"Status": "unhealthy"},
	})

	res := DockerChecker{}.Check(context.Background(), dockerMonitor(srv.URL, "web"))
	if res.Status != model.StatusDown {
		t.Fatalf("状态 = %d，期望 DOWN", res.Status)
	}
	if res.Msg != "Container State is unhealthy according to its healthcheck" {
		t.Fatalf("消息 = %q", res.Msg)
	}
}

func TestDockerNotRunning(t *testing.T) {
	srv := dockerFakeDaemon(t, map[string]any{"Status": "exited", "Running": false})

	res := DockerChecker{}.Check(context.Background(), dockerMonitor(srv.URL, "web"))
	if res.Status != model.StatusDown || res.Msg != "Container State is exited" {
		t.Fatalf("得到 %d / %q", res.Status, res.Msg)
	}
}

func TestDockerPaused(t *testing.T) {
	srv := dockerFakeDaemon(t, map[string]any{
		"Status": "paused", "Running": true, "Paused": true,
	})

	res := DockerChecker{}.Check(context.Background(), dockerMonitor(srv.URL, "web"))
	if res.Status != model.StatusDown || res.Msg != "Container is in a paused state" {
		t.Fatalf("得到 %d / %q", res.Status, res.Msg)
	}
}

func TestDockerRestartingIsPending(t *testing.T) {
	srv := dockerFakeDaemon(t, map[string]any{
		"Status": "running", "Running": true, "Restarting": true,
	})

	res := DockerChecker{}.Check(context.Background(), dockerMonitor(srv.URL, "web"))
	if res.Status != model.StatusPending {
		t.Fatalf("状态 = %d，期望 PENDING", res.Status)
	}
}

// TestDockerMissingConfig 校验没配 docker_host 时的报错文案与官方一致。
func TestDockerMissingConfig(t *testing.T) {
	m := dockerMonitor("", "web")

	res := DockerChecker{}.Check(context.Background(), m)
	if res.Status != model.StatusDown || res.Msg != "Failed to load docker host config" {
		t.Fatalf("得到 %d / %q", res.Status, res.Msg)
	}
}

func TestDockerTestDockerHost(t *testing.T) {
	srv := dockerFakeDaemon(t, nil)

	n, err := TestDockerHost(context.Background(), DockerHostInfo{Daemon: srv.URL, Type: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("容器数 = %d，期望 2", n)
	}
}

// TestDockerNonDockerResponse 对面不是 Docker 时要给出官方那句提示。
func TestDockerNonDockerResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"hello": "world"}})
	}))
	defer srv.Close()

	_, err := TestDockerHost(context.Background(), DockerHostInfo{Daemon: srv.URL, Type: "tcp"})
	if err == nil || err.Error() != "Invalid Docker response, is it Docker really a daemon?" {
		t.Fatalf("err = %v", err)
	}
}

// TestPatchDockerURL 守住 tcp:// → http:// 的运行时替换。
func TestPatchDockerURL(t *testing.T) {
	cases := map[string]string{
		"tcp://192.168.1.2:2375": "http://192.168.1.2:2375",
		"http://127.0.0.1:2376":  "http://127.0.0.1:2376",
		"https://docker.example": "https://docker.example",
	}
	for in, want := range cases {
		if got := patchDockerURL(in); got != want {
			t.Fatalf("patchDockerURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}
