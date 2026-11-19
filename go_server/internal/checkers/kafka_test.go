package checkers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

func TestParseKafkaBrokers(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`["127.0.0.1:9092"]`, 1},
		{`["a:9092","b:9092",""]`, 2},
		{`127.0.0.1:9092`, 1},
		{``, 0},
	}
	for _, c := range cases {
		if got := parseKafkaBrokers(c.raw); len(got) != c.want {
			t.Fatalf("parseKafkaBrokers(%q) = %v，期望 %d 个", c.raw, got, c.want)
		}
	}
}

func TestBuildKafkaSasl(t *testing.T) {
	if m, err := buildKafkaSasl(""); err != nil || m != nil {
		t.Fatalf("空配置应返回 nil: %v %v", m, err)
	}
	if m, err := buildKafkaSasl(`{"mechanism":"None"}`); err != nil || m != nil {
		t.Fatalf("mechanism=None 应返回 nil: %v %v", m, err)
	}

	for _, mech := range []string{"plain", "scram-sha-256", "scram-sha-512"} {
		m, err := buildKafkaSasl(`{"mechanism":"` + mech + `","username":"u","password":"p"}`)
		if err != nil || m == nil {
			t.Fatalf("%s 应构造出 mechanism: %v %v", mech, m, err)
		}
		if !strings.HasPrefix(m.Name(), strings.ToUpper(strings.Split(mech, "-")[0])) {
			t.Fatalf("%s → %s，名字不匹配", mech, m.Name())
		}
	}

	if _, err := buildKafkaSasl(`{"mechanism":"aws"}`); err == nil {
		t.Fatal("aws 机制应明确报不支持")
	}
	if _, err := buildKafkaSasl(`{"mechanism":"kerberos"}`); err == nil {
		t.Fatal("未知机制应报错")
	}
}

func TestKafkaMissingFields(t *testing.T) {
	res := KafkaProducerChecker{}.Check(context.Background(), &model.Monitor{
		Interval: 10,
		Raw:      map[string]any{},
	})
	if res.Status != model.StatusDown || res.Msg != "Brokers are required" {
		t.Fatalf("得到 %d / %q", res.Status, res.Msg)
	}

	res = KafkaProducerChecker{}.Check(context.Background(), &model.Monitor{
		Interval: 10,
		Raw:      map[string]any{"kafka_producer_brokers": `["127.0.0.1:9092"]`},
	})
	if res.Status != model.StatusDown || res.Msg != "Topic is required" {
		t.Fatalf("得到 %d / %q", res.Status, res.Msg)
	}
}

// TestKafkaUnreachable 打一个没人监听的 broker，必须快速失败且状态为 DOWN。
func TestKafkaUnreachable(t *testing.T) {
	m := &model.Monitor{
		Interval: 1,
		Raw: map[string]any{
			"kafka_producer_brokers": `["127.0.0.1:1"]`,
			"kafka_producer_topic":   "uptime-kuma",
			"kafka_producer_message": "hello",
			"timeout":                float64(1),
		},
	}

	start := time.Now()
	res := KafkaProducerChecker{}.Check(context.Background(), m)
	elapsed := time.Since(start)

	if res.Status != model.StatusDown {
		t.Fatalf("状态 = %d，期望 DOWN", res.Status)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("失败判定太慢: %v", elapsed)
	}
	t.Logf("DOWN 消息: %s（耗时 %v）", res.Msg, elapsed)
}
