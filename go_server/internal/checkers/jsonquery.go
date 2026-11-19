package checkers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	jsonata "github.com/xiatechs/jsonata-go"
)

// JSONata 求值。
//
// 官方把 jsonata 跑在 node:vm 沙箱里（server/jsonata-sandbox.js），
// 支持完整表达式：算术、$sum/$count 等函数、过滤、映射、条件。
// Go 侧用纯 Go 实现的 jsonata-go，表达式语法与官方一致，
// 因此 `$sum(payload.values)` 这类写法可以直接复用前端已有的教程。
//
// 与官方唯一的差异：guardrail（超时/递归深度）由 jsonata-go 自己的实现决定，
// 没有 1 秒强制中断。探测本身有 ctx 超时，最坏情况会被这一层兜住。

var (
	jsonataMu    sync.Mutex
	jsonataCache = map[string]*jsonata.Expr{}
)

// jsonataCacheLimit 防止用户不断改表达式导致缓存无限增长。
const jsonataCacheLimit = 512

func compileJSONata(expr string) (*jsonata.Expr, error) {
	jsonataMu.Lock()
	defer jsonataMu.Unlock()

	if e, ok := jsonataCache[expr]; ok {
		return e, nil
	}
	e, err := jsonata.Compile(expr)
	if err != nil {
		return nil, err
	}
	if len(jsonataCache) >= jsonataCacheLimit {
		jsonataCache = map[string]*jsonata.Expr{}
	}
	jsonataCache[expr] = e
	return e, nil
}

// evalJSONPath 用 JSONata 求值。
//
// 官方 evaluateJsonQuery 的三条硬规则这里都保留：
//  1. 表达式返回 undefined / null → 明确报错，不能当成 false；
//  2. 返回数组 → 报错并提示用 [0] 或聚合函数（否则用户会以为查询通过）；
//  3. 返回对象 → 报错，因为无法和期望值做原始比较。
func evalJSONPath(body []byte, path string) (any, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, errors.New("response is not valid JSON")
		}
		return doc, nil
	}

	expr, err := compileJSONata(path)
	if err != nil {
		return nil, fmt.Errorf("Error evaluating JSON query: %v", err)
	}

	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		// 非 JSON 响应按字符串处理（官方同样走 data.toString()）
		data = string(body)
	}

	out, err := expr.Eval(data)
	if err != nil {
		if errors.Is(err, jsonata.ErrUndefined) {
			return nil, errors.New("Empty or undefined response. Check query syntax and response structure")
		}
		return nil, fmt.Errorf("Error evaluating JSON query: %v", err)
	}
	if out == nil {
		return nil, errors.New("Empty or undefined response. Check query syntax and response structure")
	}

	switch v := out.(type) {
	case []any:
		raw, _ := json.Marshal(v)
		shown := string(raw)
		if len(shown) > 25 {
			shown = shown[:25] + "...]"
		}
		return nil, fmt.Errorf("JSON query returned the array %s, but a primitive value is required. "+
			"Modify your query to return a single value via [0] to get the first element or use an aggregation like $count(), $sum() or $boolean()", shown)
	case map[string]any:
		return nil, errors.New("The post-JSON query evaluated response from the server is of type object, which cannot be directly compared to the expected value")
	}

	return out, nil
}
