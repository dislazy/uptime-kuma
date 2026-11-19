// Package version 保存构建期注入的版本信息。
package version

// 通过 -ldflags "-X .../internal/version.Version=..." 注入。
var (
	// Version 语义化版本号，尽量与所对齐的官方版本一致。
	Version = "3.0.0-go.1"
	// Commit 构建时的 git 提交。
	Commit = "dev"
	// BuildTime 构建时间。
	BuildTime = "unknown"
)

// String 返回可打印的版本串。
func String() string {
	return Version + " (" + Commit + ", " + BuildTime + ")"
}
