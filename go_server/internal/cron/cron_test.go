package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) *Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q) 失败: %v", spec, err)
	}
	return s
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * * 13 *", "* * * * abc", "@nope",
	}
	for _, spec := range bad {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) 应当报错，实际通过", spec)
		}
	}
}

func TestParseAccepts(t *testing.T) {
	good := []string{
		"* * * * *", "0 3 * * *", "30 1 * * 1,3,5", "0 0 L * *",
		"*/5 * * * *", "0 2-6/2 * * *", "0 0 1 * MON", "@daily",
		"0 0 1 1 *", "5/10 * * * *", "0 0 ? * *", "0 0 1-5 * *",
	}
	for _, spec := range good {
		if _, err := Parse(spec); err != nil {
			t.Errorf("Parse(%q) 应当通过，实际报错: %v", spec, err)
		}
	}
}

func TestNextDaily(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	s := mustParse(t, "0 3 * * *")

	after := time.Date(2026, 11, 19, 10, 30, 0, 0, loc) // 已过 3 点
	got := s.Next(after)
	want := time.Date(2026, 11, 20, 3, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}

	after = time.Date(2026, 11, 19, 2, 0, 0, 0, loc) // 还没到 3 点
	got = s.Next(after)
	want = time.Date(2026, 11, 19, 3, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestNextStrictlyAfter(t *testing.T) {
	loc := time.UTC
	s := mustParse(t, "0 3 * * *")
	// after 正好落在触发点上，应返回第二天
	after := time.Date(2026, 11, 19, 3, 0, 0, 0, loc)
	got := s.Next(after)
	want := time.Date(2026, 11, 20, 3, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestNextWeekday(t *testing.T) {
	loc := time.UTC
	// 2026-11-19 是周四；只在周一/周三触发
	s := mustParse(t, "0 9 * * 1,3")

	after := time.Date(2026, 11, 19, 12, 0, 0, 0, loc) // 11-19 是周四
	got := s.Next(after)
	// 最近的触发是下一个周一 11-23（周三 11-25 更晚）
	want := time.Date(2026, 11, 23, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (weekday=%v)", got, want, got.Weekday())
	}
}

func TestLastDayOfMonth(t *testing.T) {
	loc := time.UTC
	s := mustParse(t, "0 0 L * *")

	// 2026-02 有 28 天
	after := time.Date(2026, 2, 1, 0, 0, 0, 0, loc)
	got := s.Next(after)
	want := time.Date(2026, 2, 28, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("2 月末 Next = %v, want %v", got, want)
	}

	// 闰年 2 月 29 天
	after = time.Date(2028, 2, 1, 0, 0, 0, 0, loc)
	got = s.Next(after)
	want = time.Date(2028, 2, 29, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("闰年 2 月末 Next = %v, want %v", got, want)
	}
}

func TestStep(t *testing.T) {
	loc := time.UTC
	s := mustParse(t, "*/15 * * * *")
	after := time.Date(2026, 11, 19, 10, 7, 0, 0, loc)
	got := s.Next(after)
	want := time.Date(2026, 11, 19, 10, 15, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestMonthRollover(t *testing.T) {
	loc := time.UTC
	s := mustParse(t, "0 0 1 * *") // 每月 1 号
	after := time.Date(2026, 12, 15, 0, 0, 0, 0, loc)
	got := s.Next(after)
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("跨年 Next = %v, want %v", got, want)
	}
}

func TestDomDowOrSemantics(t *testing.T) {
	loc := time.UTC
	// 13 号 或 周五 都触发
	s := mustParse(t, "0 0 13 * 5")

	// 2026-11-13 是周五；先看 12 号之后
	after := time.Date(2026, 11, 10, 0, 0, 0, 0, loc)
	got := s.Next(after)
	want := time.Date(2026, 11, 13, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}

	// 2026-11-14 之后：下一个周五是 11-20，早于 12-13
	after = time.Date(2026, 11, 14, 0, 0, 0, 0, loc)
	got = s.Next(after)
	want = time.Date(2026, 11, 20, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestMacro(t *testing.T) {
	loc := time.UTC
	s := mustParse(t, "@hourly")
	after := time.Date(2026, 11, 19, 10, 30, 0, 0, loc)
	got := s.Next(after)
	want := time.Date(2026, 11, 19, 11, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}
