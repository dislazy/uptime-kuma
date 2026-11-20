package monitor

import (
	"context"
	"strings"

	"github.com/dislazy/uptime-kuma/go_server/internal/checkers"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// checkGroup 计算分组监控的状态。
//
// 官方把 group 也当成一种 MonitorType，由调度器按 interval 轮询；
// 状态取所有启用子监控里最差的那个：
//
//	全部 UP          → UP   "All children up and running"
//	有 PENDING（含没有心跳的） → PENDING "Pending child monitors: a, b"
//	有 DOWN          → DOWN  "Child monitors down: a; pending: b"
//	没有子监控        → PENDING "Group empty"
//
// 注意：分组自身不是真实探测，它的 ping 永远是 nil（前端不显示响应时间）。
func (mgr *Manager) checkGroup(ctx context.Context, mon *model.Monitor) checkers.Result {
	children, err := mgr.mon.Children(ctx, mon.ID)
	if err != nil {
		return checkers.Down("Failed to load group children: %v", err)
	}

	if len(children) == 0 {
		return checkers.Pending("Group empty")
	}

	worst := model.StatusUp
	hasUp := false
	var down, pending []string

	for _, child := range children {
		if !child.Active {
			// 暂停的子监控不参与判定，否则分组会永远停在它的旧状态上
			continue
		}

		label := child.Name
		if strings.TrimSpace(label) == "" {
			label = "#" + itoa(child.ID)
		}

		beats, err := mgr.beats.Recent(ctx, child.ID, 1)
		if err != nil || len(beats) == 0 {
			if worst == model.StatusUp {
				worst = model.StatusPending
			}
			pending = append(pending, label)
			continue
		}

		switch beats[len(beats)-1].Status {
		case model.StatusDown:
			worst = model.StatusDown
			down = append(down, label)
		case model.StatusPending:
			if worst != model.StatusDown {
				worst = model.StatusPending
			}
			pending = append(pending, label)
		case model.StatusUp:
			hasUp = true
		}
	}

	if !hasUp && len(down) == 0 && len(pending) == 0 {
		// 子监控全都暂停了
		return checkers.Pending("Group empty")
	}

	switch worst {
	case model.StatusUp:
		return checkers.Up("All children up and running", 0)
	case model.StatusPending:
		return checkers.Pending("Pending child monitors: " + strings.Join(pending, ", "))
	default:
		msg := "Child monitors down: " + strings.Join(down, ", ")
		if len(pending) > 0 {
			msg += "; pending: " + strings.Join(pending, ", ")
		}
		return checkers.Down("%s", msg)
	}
}

// itoa 避免为了一处格式化引入 strconv 之外的工具。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
