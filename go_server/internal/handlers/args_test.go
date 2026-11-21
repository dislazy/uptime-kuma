package handlers

import (
	"encoding/json"
	"testing"
)

// TestArgAcceptsNumericStringID 守住「路由参数当 ID 传」这条链路。
//
// src/pages/Details.vue 与 src/pages/EditMonitor.vue 都是：
//
//	socket.emit("getMonitor", this.$route.params.id, (res) => { if (res.ok) ... })
//
// vue-router 的 params.id 是字符串 "22"。官方 Node 后端靠 SQL 隐式转换能吃下，
// Go 的 json.Unmarshal 会把 `"22"` → int64 判为错误，于是 getMonitor 回
// {ok:false}，详情页 v-if="monitor" 永远为假 —— 整个右栏空白。
func TestArgAcceptsNumericStringID(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "数字", raw: `22`, want: 22},
		{name: "数字字符串", raw: `"22"`, want: 22},
		{name: "带空白的数字字符串", raw: `" 22 "`, want: 22},
		{name: "负数", raw: `"-3"`, want: -3},
		{name: "非数字字符串", raw: `"abc"`, wantErr: true},
		{name: "空字符串", raw: `""`, wantErr: true},
		{name: "小数", raw: `"2.5"`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Ctx{Args: []json.RawMessage{json.RawMessage(tc.raw)}}

			var id int64
			err := c.Arg(0, &id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 id=%d", id)
				}
				return
			}
			if err != nil {
				t.Fatalf("Arg 报错: %v", err)
			}
			if id != tc.want {
				t.Fatalf("id = %d，期望 %d", id, tc.want)
			}

			// OptArg 走同一条解析路径，行为必须一致
			var opt int64
			if !c.OptArg(0, &opt) || opt != tc.want {
				t.Fatalf("OptArg = (%d, %v)，期望 %d", opt, c.OptArg(0, &opt), tc.want)
			}
		})
	}
}

// TestArgDoesNotLoosenNonIntegerTargets 确保宽松解析只作用于整数目标：
// 字符串参数（例如密码、名称）不能被悄悄转成数字。
func TestArgDoesNotLoosenNonIntegerTargets(t *testing.T) {
	c := &Ctx{Args: []json.RawMessage{json.RawMessage(`"22"`)}}

	var s string
	if err := c.Arg(0, &s); err != nil || s != "22" {
		t.Fatalf("字符串参数解析异常: s=%q err=%v", s, err)
	}

	var obj map[string]any
	if err := c.Arg(0, &obj); err == nil {
		t.Fatalf("字符串不应能解析成对象")
	}
}
