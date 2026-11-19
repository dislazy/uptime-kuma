package checkers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// KafkaProducerChecker 连一次 Kafka 并投递一条消息。
//
// 官方用 kafkajs：connect → send({topic, messages:[{value}]}) → 成功返回
// "Message sent successfully"。超时是 interval * 0.8，超时文案就是 "Timeout"。
// 这里用 franz-go（纯 Go，无 CGO）对齐行为与文案。
type KafkaProducerChecker struct{}

// Type 实现 Checker。
func (KafkaProducerChecker) Type() string { return "kafka-producer" }

func init() { Register(KafkaProducerChecker{}) }

// kafkaSaslOptions 对应官方 kafkaProducerSaslOptions 的 JSON 结构（kafkajs 格式）。
type kafkaSaslOptions struct {
	Mechanism string `json:"mechanism"`
	Username  string `json:"username"`
	Password  string `json:"password"`
}

// buildKafkaSasl 把 kafkajs 的 mechanism 映射到 franz-go 的 sasl.Mechanism。
// mechanism 为空或 "None" 时返回 nil（官方同样把 "None" 置为 undefined）。
func buildKafkaSasl(raw string) (sasl.Mechanism, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var opts kafkaSaslOptions
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		return nil, err
	}

	mech := strings.ToLower(strings.TrimSpace(opts.Mechanism))
	auth := saslAuth(opts.Username, opts.Password)

	switch mech {
	case "", "none":
		return nil, nil
	case "plain":
		return plain.Auth{User: auth.user, Pass: auth.pass}.AsMechanism(), nil
	case "scram-sha-256":
		return scram.Auth{User: auth.user, Pass: auth.pass}.AsSha256Mechanism(), nil
	case "scram-sha-512":
		return scram.Auth{User: auth.user, Pass: auth.pass}.AsSha512Mechanism(), nil
	case "aws":
		return nil, errors.New("AWS IAM SASL mechanism is not supported by the Go server")
	default:
		return nil, errors.New("Unsupported SASL mechanism: " + opts.Mechanism)
	}
}

type kafkaAuth struct{ user, pass string }

func saslAuth(user, pass string) kafkaAuth { return kafkaAuth{user: user, pass: pass} }

// parseKafkaBrokers 解析 broker 列表（JSON 数组），兼容单个字符串。
func parseKafkaBrokers(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		out := make([]string, 0, len(list))
		for _, b := range list {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
		return out
	}
	if raw != "" {
		return []string{raw}
	}
	return nil
}

// Check 实现 Checker。
func (KafkaProducerChecker) Check(ctx context.Context, m *model.Monitor) Result {
	brokers := parseKafkaBrokers(m.Get("kafka_producer_brokers"))
	if len(brokers) == 0 {
		return Down("Brokers are required")
	}
	topic := m.Get("kafka_producer_topic")
	if topic == "" {
		return Down("Topic is required")
	}

	saslMech, err := buildKafkaSasl(m.Get("kafka_producer_sasl_options"))
	if err != nil {
		return Down("%s", err.Error())
	}

	// 总超时：官方 interval * 1000 * 0.8，超时文案固定 "Timeout"
	totalTimeout := time.Duration(float64(m.Interval) * 0.8 * float64(time.Second))
	if totalTimeout <= 0 {
		totalTimeout = 16 * time.Second
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ClientID("Uptime-Kuma"),
		kgo.RecordRetries(0),
		kgo.RequestRetries(0),
	}
	if m.GetBool("kafka_producer_allow_auto_topic_creation") {
		opts = append(opts, kgo.AllowAutoTopicCreation())
	}
	if v := m.GetFloat("timeout"); v > 0 {
		opts = append(opts, kgo.DialTimeout(time.Duration(v*float64(time.Second))))
	}
	if saslMech != nil {
		opts = append(opts, kgo.SASL(saslMech))
	}
	if m.GetBool("kafka_producer_ssl") {
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}

	start := time.Now()

	runCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return Down("Error in producer connection: %s", err.Error())
	}
	defer cl.Close()

	rec := &kgo.Record{Topic: topic, Value: []byte(m.Get("kafka_producer_message"))}
	results := cl.ProduceSync(runCtx, rec)
	if err := results.FirstErr(); err != nil {
		if runCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return Down("Timeout")
		}
		return Down("Error sending message: %s", err.Error())
	}

	return Up("Message sent successfully", time.Since(start).Milliseconds())
}
