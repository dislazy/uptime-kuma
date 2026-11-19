package checkers

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/songbo/uptime-kuma/go_server/internal/model"
)

// MongoDBChecker 在数据库上执行一条命令（默认 ping）。
type MongoDBChecker struct{}

// Type 实现 Checker。
func (MongoDBChecker) Type() string { return "mongodb" }

func init() { Register(MongoDBChecker{}) }

// Check 执行命令并按 ok 字段判定，可选再用 JSON 路径做值校验。
func (MongoDBChecker) Check(ctx context.Context, m *model.Monitor) Result {
	uri := strings.TrimSpace(m.Get("database_connection_string"))
	if uri == "" {
		return Down("Connection string is empty")
	}

	command := bson.D{{Key: "ping", Value: 1}}
	if raw := strings.TrimSpace(m.Get("database_query")); raw != "" {
		var parsed bson.D
		if err := bson.UnmarshalExtJSON([]byte(raw), true, &parsed); err != nil {
			return Down("Invalid database query: %s", err.Error())
		}
		command = parsed
	}

	start := time.Now()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return Down("%s", err.Error())
	}
	defer func() {
		// 断开要用独立的 ctx：探测 ctx 此时可能已超时，否则连接泄漏到下一轮
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(closeCtx)
	}()

	var result bson.M
	if err := client.Database(mongoDatabaseFromURI(uri)).RunCommand(ctx, command).Decode(&result); err != nil {
		return Down("%s", err.Error())
	}

	ping := time.Since(start).Milliseconds()

	if ok, _ := toFloat(result["ok"]); ok != 1 {
		return Down("MongoDB command failed")
	}

	msg := "Command executed successfully"

	// 把结果转成 JSON，供 jsonPath 求值与 expectedValue 比较复用同一套逻辑
	raw, err := json.Marshal(result)
	if err != nil {
		return Up(msg, ping)
	}

	var value any = result
	if path := strings.TrimSpace(m.Get("json_path")); path != "" {
		v, err := evalJSONPath(raw, path)
		if err != nil {
			return Down("%s", err.Error())
		}
		value = v
		// 官方语义：表达式求不出结果（nil/false/空）即视为失败
		if v == nil || v == false {
			return Down("Queried value not found.")
		}
		msg = "Command executed successfully and the jsonata expression produces a result."
	}

	if expected := strings.TrimSpace(m.Get("expected_value")); expected != "" {
		if stringify(value) == expected {
			msg = "Command executed successfully and expected value was found"
		} else {
			return Down("Query executed, but value is not equal to expected value, value was: [%s]", stringify(value))
		}
	}

	return Up(msg, ping)
}

// mongoDatabaseFromURI 从连接串里取数据库名，缺省用 test（与官方 client.db() 行为一致）。
func mongoDatabaseFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return "test"
	}
	name := strings.TrimPrefix(u.Path, "/")
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return "test"
	}
	return name
}
