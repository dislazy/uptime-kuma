// Package logx 提供带组件标签的轻量日志。
// 单二进制场景下不引入第三方日志库，stdout 结构化输出即可交给 docker logs 收集。
package logx

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	current atomic.Int32
	mu      sync.Mutex
)

// SetDebug 打开/关闭 debug 级别。
func SetDebug(on bool) {
	if on {
		current.Store(int32(LevelDebug))
		return
	}
	current.Store(int32(LevelInfo))
}

func levelTag(l Level) string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelWarn:
		return "WARN "
	case LevelError:
		return "ERROR"
	default:
		return "INFO "
	}
}

func output(l Level, component, format string, args ...any) {
	if l < Level(current.Load()) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05.000"), levelTag(l), component, msg)

	mu.Lock()
	_, _ = os.Stdout.WriteString(line)
	mu.Unlock()
}

// Debugf 输出 debug 日志。
func Debugf(component, format string, args ...any) { output(LevelDebug, component, format, args...) }

// Infof 输出 info 日志。
func Infof(component, format string, args ...any) { output(LevelInfo, component, format, args...) }

// Warnf 输出 warn 日志。
func Warnf(component, format string, args ...any) { output(LevelWarn, component, format, args...) }

// Errorf 输出 error 日志。
func Errorf(component, format string, args ...any) { output(LevelError, component, format, args...) }
