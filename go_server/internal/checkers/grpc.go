package checkers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoparse"
	"github.com/jhump/protoreflect/dynamic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/dislazy/uptime-kuma/go_server/internal/model"
)

// GRPCChecker 按用户给的 .proto 动态构造请求，调一次 unary 方法再做 keyword 断言。
//
// 官方用 protobufjs + @grpc/grpc-js 在运行时解析 proto。Go 侧用
// jhump/protoreflect 的 protoparse 做同样的事：解析 proto 文本 → 拿
// MethodDescriptor → 用 dynamic.Message 做 JSON ↔ protobuf 转换。
//
// 协议层不走 grpc 默认 codec（它是 protobuf v2 接口，dynamic.Message 是 v1），
// 而是自己 Marshal 成 wire 字节再配一个 raw codec，行为等价且不做二次编解码。
type GRPCChecker struct{}

// Type 实现 Checker。
func (GRPCChecker) Type() string { return "grpc-keyword" }

func init() { Register(GRPCChecker{}) }

// rawCodec 直接把已编码好的 protobuf 字节塞给 grpc。
type rawCodec struct{}

type rawMessage struct{ data []byte }

func (rawCodec) Marshal(v any) ([]byte, error) {
	m, ok := v.(*rawMessage)
	if !ok {
		return nil, fmt.Errorf("rawCodec: unexpected type %T", v)
	}
	return m.data, nil
}

func (rawCodec) Unmarshal(b []byte, v any) error {
	m, ok := v.(*rawMessage)
	if !ok {
		return fmt.Errorf("rawCodec: unexpected type %T", v)
	}
	m.data = append([]byte(nil), b...)
	return nil
}

func (rawCodec) Name() string { return "proto" }

// Check 实现 Checker。
func (GRPCChecker) Check(ctx context.Context, m *model.Monitor) Result {
	target := strings.TrimSpace(m.Get("grpc_url"))
	if target == "" {
		return Down("gRPC URL is required")
	}
	// 允许用户直接粘 http(s):// 前缀
	target = strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")

	protoText := m.Get("grpc_protobuf")
	if strings.TrimSpace(protoText) == "" {
		return Down("gRPC Protobuf is required")
	}
	serviceName := strings.TrimSpace(m.Get("grpc_service_name"))
	if serviceName == "" {
		return Down("gRPC Service Name is required")
	}
	methodName := strings.TrimSpace(m.Get("grpc_method"))
	if methodName == "" {
		return Down("gRPC Method is required")
	}

	files, err := parseProto(protoText)
	if err != nil {
		return Down("Failed to parse protobuf: %v", err)
	}

	service := findService(files, serviceName)
	if service == nil {
		return Down("Service %q not found in protobuf definition", serviceName)
	}
	method := findMethod(service, methodName)
	if method == nil {
		return Down("Method %q not found in service %q", methodName, serviceName)
	}

	body := m.Get("grpc_body")
	if strings.TrimSpace(body) == "" {
		body = "{}"
	}
	req := dynamic.NewMessage(method.GetInputType())
	if err := req.UnmarshalJSON([]byte(body)); err != nil {
		return Down("Failed to parse gRPC request body: %v", err)
	}
	reqBytes, err := req.Marshal()
	if err != nil {
		return Down("Failed to encode gRPC request: %v", err)
	}

	useTLS := m.GetBool("grpc_enable_tls")
	var creds credentials.TransportCredentials
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		creds = insecure.NewCredentials()
	}

	dialCtx, cancel := context.WithTimeout(ctx, Timeout(m))
	defer cancel()

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	)
	if err != nil {
		return Down("%s", err.Error())
	}
	defer conn.Close()

	callCtx := dialCtx
	if md := grpcMetadata(m.Get("grpc_metadata")); len(md) > 0 {
		callCtx = metadata.NewOutgoingContext(callCtx, md)
	}

	respMsg := dynamic.NewMessage(method.GetOutputType())
	out := &rawMessage{}
	fullMethod := "/" + service.GetFullyQualifiedName() + "/" + method.GetName()

	start := time.Now()
	err = conn.Invoke(callCtx, fullMethod, &rawMessage{data: reqBytes}, out)
	ping := time.Since(start).Milliseconds()

	var response string
	if err != nil {
		// 官方把 code=1（CANCELLED）当作成功，因为服务端常常主动取消长请求
		if strings.Contains(err.Error(), "code = Canceled") || strings.Contains(err.Error(), "code = 1") {
			response = fmt.Sprintf("%s is considered OK because %s", "1", err.Error())
		} else {
			return Down("%s", err.Error())
		}
	} else {
		if err := respMsg.Unmarshal(out.data); err != nil {
			return Down("Failed to decode gRPC response: %v", err)
		}
		js, err := respMsg.MarshalJSON()
		if err != nil {
			return Down("Failed to serialize gRPC response: %v", err)
		}
		response = string(js)
	}

	keyword := m.Get("keyword")
	found := strings.Contains(response, keyword)
	if found != !m.GetBool("invert_keyword") {
		shown := response
		if len(shown) > 50 {
			shown = shown[:47] + "..."
		}
		state := "not"
		if found {
			state = "present"
		}
		return Down("keyword [%s] is %s in [%s]", keyword, state, shown)
	}

	state := "not found"
	if found {
		state = "found"
	}
	return Up(fmt.Sprintf("%s, keyword [%s] %s", response, keyword, state), ping)
}

// findService 在所有解析出的文件（含 import）里按全名或短名找服务。
func findService(files []*desc.FileDescriptor, name string) *desc.ServiceDescriptor {
	name = strings.TrimPrefix(name, ".")
	for _, f := range files {
		for _, svc := range f.GetServices() {
			if svc.GetFullyQualifiedName() == name || svc.GetName() == name {
				return svc
			}
		}
		for _, dep := range f.GetDependencies() {
			if svc := findService([]*desc.FileDescriptor{dep}, name); svc != nil {
				return svc
			}
		}
	}
	return nil
}

// findMethod 方法与官方一致按名字匹配，额外容忍大小写差异（proto 里常写 CamelCase，
// 用户从文档里复制过来可能是 camelCase）。
func findMethod(svc *desc.ServiceDescriptor, name string) *desc.MethodDescriptor {
	if md := svc.FindMethodByName(name); md != nil {
		return md
	}
	for _, md := range svc.GetMethods() {
		if strings.EqualFold(md.GetName(), name) {
			return md
		}
	}
	return nil
}

// grpcMetadata 解析前端传来的 metadata。
// 官方表单允许 JSON 对象（{"k":"v"}）或数组（[{"key":..,"value":..}]），两种都吃。
func grpcMetadata(raw string) metadata.MD {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var asMap map[string]string
	if err := json.Unmarshal([]byte(raw), &asMap); err == nil && len(asMap) > 0 {
		return metadata.New(asMap)
	}

	var asList []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &asList); err == nil {
		md := metadata.MD{}
		for _, kv := range asList {
			if kv.Key != "" {
				md.Append(kv.Key, kv.Value)
			}
		}
		if len(md) > 0 {
			return md
		}
	}

	// 最后尝试 k=v 换行分隔
	md := metadata.MD{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		md.Append(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	if len(md) == 0 {
		return nil
	}
	return md
}

// parseProto 把用户粘进来的 .proto 文本解析成描述符（含 import 的依赖文件）。
func parseProto(protoText string) ([]*desc.FileDescriptor, error) {
	return protoparse.Parser{
		Accessor: protoparse.FileContentsFromMap(map[string]string{"monitor.proto": protoText}),
	}.ParseFiles("monitor.proto")
}
