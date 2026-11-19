// Package conditions 复刻官方的监控条件（conditions）系统。
//
// 用户在编辑监控时可以配置一组「变量 + 操作符 + 期望值」的表达式，
// 探测完成后按表达式判定这次到底算不算成功。例如 MySQL 返回的单值
// 必须等于某个数、MQTT 消息必须包含某个关键字。
//
// 表达式结构（与前端存进 monitor.conditions 的 JSON 一致）：
//
//	[{"type":"expression","variable":"result","operator":"equals","value":"1","andOr":"and"}]
//	[{"type":"group","andOr":"and","children":[...]}]
//
// 注意 andOr 挂在「子节点」上，表示它与**前一个**结果如何结合；
// 第一个子节点的 andOr 被忽略。这与官方实现保持一致。
package conditions

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// andOr 取值
const (
	logicalAnd = "and"
	logicalOr  = "or"
)

// Expr 是条件树的一个节点：表达式或分组。
type Expr struct {
	Type     string `json:"type"`
	Variable string `json:"variable"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
	AndOr    string `json:"andOr"`
	Children []Expr `json:"children"`
}

// Eval 对 monitor.conditions 的原始 JSON 求值。
//
// 未配置条件（空串 / null / 空数组）时返回 true——调用方可以无条件使用，
// 不需要自己判断"有没有配条件"。
func Eval(raw string, vars map[string]any) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "[]" {
		return true, nil
	}

	var nodes []Expr
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return false, fmt.Errorf("invalid conditions: %w", err)
	}
	if len(nodes) == 0 {
		return true, nil
	}
	return evalGroup(nodes, vars)
}

// Has 判断条件 JSON 里是否真的配了表达式（空数组视为未配置）。
func Has(raw string) bool {
	raw = strings.TrimSpace(raw)
	return raw != "" && raw != "null" && raw != "[]"
}

func evalGroup(children []Expr, vars map[string]any) (bool, error) {
	if len(children) == 0 {
		return false, fmt.Errorf("condition group must contain at least one child")
	}

	var (
		result bool
		first  = true
	)

	for _, ch := range children {
		var (
			cur bool
			err error
		)
		if ch.Type == "group" {
			cur, err = evalGroup(ch.Children, vars)
		} else {
			cur, err = evalExpr(ch, vars)
		}
		if err != nil {
			return false, err
		}

		if first {
			result = cur
			first = false
			continue
		}

		switch strings.ToLower(ch.AndOr) {
		case logicalOr:
			result = result || cur
		case logicalAnd, "":
			result = result && cur
		default:
			return false, fmt.Errorf("invalid logical operator %q, expected 'and' or 'or'", ch.AndOr)
		}
	}

	return result, nil
}

func evalExpr(e Expr, vars map[string]any) (bool, error) {
	v, ok := vars[e.Variable]
	if !ok {
		return false, fmt.Errorf("variable missing in context: %s", e.Variable)
	}
	return testOperator(e.Operator, v, e.Value)
}

// testOperator 按官方 14 个操作符的语义判定。
//
// 这里的比较语义刻意贴近 JS：
//   - equals 是严格相等（字符串只和字符串比，数字只和数字比）
//   - contains / starts_with / ends_with 作用于字符串，contains 另外支持数组
//   - num_* / lt / gt / lte / gte 要求变量本身是数字，字符串变量一律不成立
func testOperator(op string, variable, expected any) (bool, error) {
	switch op {
	case "equals":
		return strictEquals(variable, expected), nil

	case "not_equals":
		return !strictEquals(variable, expected), nil

	case "contains":
		return containsValue(variable, expected), nil

	case "not_contains":
		return !containsValue(variable, expected), nil

	case "starts_with":
		s, ok := variable.(string)
		return ok && strings.HasPrefix(s, toString(expected)), nil

	case "not_starts_with":
		s, ok := variable.(string)
		return ok && !strings.HasPrefix(s, toString(expected)), nil

	case "ends_with":
		s, ok := variable.(string)
		return ok && strings.HasSuffix(s, toString(expected)), nil

	case "not_ends_with":
		s, ok := variable.(string)
		return ok && !strings.HasSuffix(s, toString(expected)), nil

	case "num_equals", "num_not_equals", "lt", "gt", "lte", "gte":
		left, ok := numericValue(variable)
		if !ok {
			return false, nil
		}
		right, ok := numericValue(expected)
		if !ok {
			right, ok = numericValue(toString(expected))
			if !ok {
				return false, nil
			}
		}

		switch op {
		case "num_equals":
			return left == right, nil
		case "num_not_equals":
			return left != right, nil
		case "lt":
			return left < right, nil
		case "gt":
			return left > right, nil
		case "lte":
			return left <= right, nil
		default: // gte
			return left >= right, nil
		}

	default:
		return false, fmt.Errorf("unexpected expression operator %q", op)
	}
}

// strictEquals 模拟 JS 的 ===：类型不同直接不相等。
func strictEquals(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := toFloat(b)
		return ok && av == bv
	case int:
		bv, ok := toFloat(b)
		return ok && float64(av) == bv
	case int64:
		bv, ok := toFloat(b)
		return ok && float64(av) == bv
	default:
		return false
	}
}

func containsValue(variable, expected any) bool {
	switch v := variable.(type) {
	case string:
		return strings.Contains(v, toString(expected))
	case []any:
		for _, item := range v {
			if strictEquals(item, expected) {
				return true
			}
		}
		return false
	}
	return false
}

// numericValue 只在变量确实是数字类型时返回成功，字符串不参与数值比较。
func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func toFloat(v any) (float64, bool) { return numericValue(v) }

// toString 把期望值转成字符串。前端存的多数是字符串，
// 但也可能是数字（比如 JSON 路径的期望值是 5 而不是 "5"）。
func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", t)
	}
}
