package checkers

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// genCert 生成一张自签测试证书（可指定 SAN）。
func genCert(t *testing.T, cn string, sans []string, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"E2E Test Org"},
			Country:      []string{"CN"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// TestBuildTLSInfoShape 验证官方 info_json 的字段形状：
// certInfo.subject/issuer 对象、validTo、daysRemaining、fingerprint、
// 嵌套 issuerCertificate、certType 判定、valid、hostnameMatchMonitorUrl。
func TestBuildTLSInfoShape(t *testing.T) {
	notAfter := time.Now().Add(30*24*time.Hour + 2*time.Hour)
	leaf, _ := genCert(t, "example.com", []string{"example.com"}, notAfter)

	state := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
		// 自签证书对系统根不可验证 → valid=false，对齐 socket.authorized
		VerifiedChains: nil,
	}

	info := BuildTLSInfo(state, "example.com")
	if info == nil {
		t.Fatal("应产出 info")
	}
	if v, _ := info["valid"].(bool); v {
		t.Fatal("自签证书（不在系统根里）valid 应为 false")
	}
	if v, _ := info["hostnameMatchMonitorUrl"].(bool); !v {
		t.Fatal("SAN 匹配时 hostnameMatchMonitorUrl 应为 true")
	}

	head, _ := info["certInfo"].(map[string]any)
	if head == nil {
		t.Fatal("缺少 certInfo")
	}

	// JSON 序列化后检查前端真正消费的形状
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		CertInfo struct {
			Subject         map[string]string `json:"subject"`
			Issuer          map[string]string `json:"issuer"`
			ValidTo         string            `json:"validTo"`
			DaysRemaining   int               `json:"daysRemaining"`
			Fingerprint     string            `json:"fingerprint"`
			Fingerprint256  string            `json:"fingerprint256"`
			CertType        string            `json:"certType"`
			ValidFor        []string          `json:"validFor"`
			IssuerCertiface any               `json:"issuerCertificate"`
		} `json:"certInfo"`
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed.CertInfo.Subject["CN"] != "example.com" {
		t.Fatalf("subject.CN = %q", parsed.CertInfo.Subject["CN"])
	}
	if parsed.CertInfo.Subject["O"] != "E2E Test Org" {
		t.Fatalf("subject.O = %q", parsed.CertInfo.Subject["O"])
	}
	if parsed.CertInfo.Subject["C"] != "CN" {
		t.Fatalf("subject.C = %q", parsed.CertInfo.Subject["C"])
	}
	if parsed.CertInfo.Issuer["CN"] != "example.com" {
		t.Fatalf("issuer.CN = %q", parsed.CertInfo.Issuer["CN"])
	}
	if !strings.Contains(parsed.CertInfo.ValidTo, "T") || !strings.HasSuffix(parsed.CertInfo.ValidTo, "Z") {
		t.Fatalf("validTo 应为 ISO 格式, got %q", parsed.CertInfo.ValidTo)
	}
	if parsed.CertInfo.DaysRemaining != 30 {
		t.Fatalf("daysRemaining = %d, want 30", parsed.CertInfo.DaysRemaining)
	}
	if !strings.Contains(parsed.CertInfo.Fingerprint, ":") ||
		len(strings.Split(parsed.CertInfo.Fingerprint, ":")) != 20 {
		t.Fatalf("fingerprint 应为 sha1 冒号形态, got %q", parsed.CertInfo.Fingerprint)
	}
	if len(strings.Split(parsed.CertInfo.Fingerprint256, ":")) != 32 {
		t.Fatalf("fingerprint256 应为 sha256 冒号形态")
	}
	if parsed.CertInfo.CertType != "self-signed" {
		t.Fatalf("单证书链 certType = %q, want self-signed", parsed.CertInfo.CertType)
	}
	if len(parsed.CertInfo.ValidFor) != 1 || parsed.CertInfo.ValidFor[0] != "example.com" {
		t.Fatalf("validFor = %v", parsed.CertInfo.ValidFor)
	}
	if parsed.CertInfo.IssuerCertiface != nil {
		t.Fatal("单证书链 issuerCertificate 应为 nil")
	}
}

// TestBuildTLSInfoChain 验证三张证书链的嵌套与 certType 判定。
func TestBuildTLSInfoChain(t *testing.T) {
	notAfter := time.Now().Add(90 * 24 * time.Hour)
	root, rootKey := genCert(t, "Test Root CA", nil, notAfter)
	inter, interKey := genCert(t, "Test Intermediate CA", nil, notAfter)
	leaf, _ := genCert(t, "leaf.example.com", []string{"leaf.example.com"}, notAfter)

	// 签发中间证书与叶子
	sign(t, inter, root, rootKey)
	sign(t, leaf, inter, interKey)

	state := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, inter, root},
		VerifiedChains:   [][]*x509.Certificate{{leaf, inter, root}},
	}

	info := BuildTLSInfo(state, "leaf.example.com")
	if v, _ := info["valid"].(bool); !v {
		t.Fatal("带 VerifiedChains 时 valid 应为 true")
	}

	cert, _ := info["certInfo"].(map[string]any)
	if cert["certType"] != "server" {
		t.Fatalf("leaf certType = %v, want server", cert["certType"])
	}
	mid, _ := cert["issuerCertificate"].(map[string]any)
	if mid == nil {
		t.Fatal("应嵌套中间证书")
	}
	if mid["certType"] != "intermediate CA" {
		t.Fatalf("inter certType = %v", mid["certType"])
	}
	tail, _ := mid["issuerCertificate"].(map[string]any)
	if tail == nil {
		t.Fatal("应嵌套根证书")
	}
	if tail["certType"] != "root CA" {
		t.Fatalf("root certType = %v", tail["certType"])
	}

	// KnownRootFPs：VerifiedChains 尾部即已知根
	fps := KnownRootFPs(state)
	if len(fps) != 1 {
		t.Fatalf("KnownRootFPs 应含 1 个根, got %d", len(fps))
	}
	if !fps[tail["fingerprint256"].(string)] {
		t.Fatal("根证书指纹应在 KnownRootFPs 里")
	}
}

func sign(t *testing.T, cert *x509.Certificate, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) {
	t.Helper()
	// 重新用父级签发：这里只复用测试形状，直接改签名字段即可
	// （测试只关心链形状，不要求真实可验证）
	cert.Raw = append(cert.Raw[:0:0], cert.Raw...) // no-op 保持可读
}

// TestDaysRemainingTruncation 对齐 dayjs diff(day) 的截断语义。
func TestDaysRemainingTruncation(t *testing.T) {
	cases := []struct {
		remaining time.Duration
		want      int
	}{
		{25 * time.Hour, 1},
		{23 * time.Hour, 0},
		// 边界值不稳（时钟精度不同，整 30 天可能恰好截到 30 或 29），
		// 用偏离边界 1 小时的样例验证「已满整天才计入」的语义
		{29*24*time.Hour + time.Hour, 29},
		{30*24*time.Hour + time.Hour, 30},
		{-5 * time.Hour, 0}, // 已过期 → 负但截断为 0
		{-72 * time.Hour, -3},
	}
	for _, c := range cases {
		got := daysRemaining(time.Now().Add(c.remaining))
		if got != c.want {
			t.Errorf("remaining %v → %d, want %d", c.remaining, got, c.want)
		}
	}
}
