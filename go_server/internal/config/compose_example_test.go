package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// composePath 是仓库里的示例编排，相对本包目录。
const composePath = "../../docker-compose.example.yml"

// TestComposeExampleUsesKnownEnvVars 保证示例 compose 里的 UPTIME_KUMA_* 变量
// 都能被 config 包识别。
//
// 为什么值得单独测：写错一个变量名（例如 UPTIME_KUMA_DB_HOST 少了 NAME）不会有
// 任何报错，程序会安静地用默认值启动——DB_TYPE 拼错时更糟，直接回落到 SQLite，
// 界面上看是"全新安装"，很容易被当成数据丢了。
func TestComposeExampleUsesKnownEnvVars(t *testing.T) {
	raw, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", composePath, err)
	}

	// 收集本包所有非测试源码，拼成一个大字符串用于查找变量名
	var src strings.Builder
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列出源码失败: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		src.Write(b)
	}
	code := src.String()

	re := regexp.MustCompile(`UPTIME_KUMA_[A-Z0-9_]+`)
	seen := map[string]bool{}
	for _, name := range re.FindAllString(string(raw), -1) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(code, `"`+name+`"`) {
			t.Errorf("compose 里用了 %s，但 config 包没有任何代码读取它（拼写错误会导致静默回落默认值）", name)
		}
	}

	if len(seen) < 5 {
		t.Fatalf("只从 compose 里解析出 %d 个变量，解析逻辑可能失效", len(seen))
	}
}

// TestComposeExampleKeepsMariaDBExternal 锁住"数据库不做进镜像"这个决定：
// 示例必须是应用 + 独立 db 服务，并且等 db 健康后才启动应用。
func TestComposeExampleKeepsMariaDBExternal(t *testing.T) {
	raw, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", composePath, err)
	}
	body := string(raw)

	checks := []struct {
		what string
		want string
	}{
		{"应用使用 mariadb 后端", "UPTIME_KUMA_DB_TYPE: mariadb"},
		{"数据库主机指向 compose 服务名", "UPTIME_KUMA_DB_HOSTNAME: db"},
		{"等待数据库健康", "condition: service_healthy"},
		{"独立的 db 服务", "\n  db:\n"},
		{"MariaDB 镜像", "image: mariadb:"},
	}
	for _, c := range checks {
		if !strings.Contains(body, c.want) {
			t.Errorf("compose 缺少「%s」（期望包含 %q）", c.what, c.want)
		}
	}

	// 注释里提到 mariadbd 是解释"为什么不这么做"，真正要禁的是可执行配置里出现它：
	// 比如 command 里起 mariadbd、或把官方 embedded 模式的变量塞进来。
	for i, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, bad := range []string{"mariadbd", "embedded-mariadb", "UPTIME_KUMA_DB_EMBEDDED"} {
			if strings.Contains(trimmed, bad) {
				t.Errorf("第 %d 行在有效配置里引用了 %q：数据库必须是独立容器，不能塞进应用镜像", i+1, bad)
			}
		}
	}
}
