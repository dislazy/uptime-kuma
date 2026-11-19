package checkers

import "context"

// SettingsProvider 让检查器读取全局设置。
//
// 少数监控类型必须依赖全局配置才能工作：
//
//	steam        → steamAPIKey
//	globalping   → globalpingApiToken
//	real-browser → chromeExecutable
//
// 这里用注入而不是直接依赖 store 包，避免 checkers → store → model 的依赖链
// 把最底层的探测逻辑和数据库绑死。
type SettingsProvider interface {
	GetString(ctx context.Context, key, def string) string
}

var globalSettings SettingsProvider

// SetSettings 注入全局设置读取器，由 main 在启动时调用一次。
func SetSettings(p SettingsProvider) { globalSettings = p }

// Setting 读取全局设置项；未注入或键不存在时返回 def。
func Setting(ctx context.Context, key, def string) string {
	if globalSettings == nil {
		return def
	}
	if v := globalSettings.GetString(ctx, key, ""); v != "" {
		return v
	}
	return def
}
