package checkers

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TLS 证书信息采集，对齐官方 server/monitor-tls.ts readTlsInfo +
// server/util.ts parseCertificateInfo 的输出形状。
//
// 官方链路：https 监控的 TLS 握手完成后读 socket.getPeerCertificate(true)，
// 加工成 { certInfo: <证书链头>, valid, hostnameMatchMonitorUrl } 并
// JSON.stringify 存入 monitor_tls_info.info_json，前端 CertificateInfoRow
// 直接渲染这些字段（subject/issuer 对象、validTo、daysRemaining、
// fingerprint、嵌套 issuerCertificate、certType）。
//
// 与官方的已知微偏差：官方 JSON 里带原始证书字节（Node Buffer 序列化），
// Go 侧省略——前端不消费 raw，跨实现读库也不依赖它，只省存储。

// BuildTLSInfo 从一次 TLS 握手构建官方形状的 tlsInfo map。
//
// hostname 为监控 URL 的主机名（官方传 getUrl()?.hostname）。
func BuildTLSInfo(state *tls.ConnectionState, hostname string) map[string]any {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}

	leaf := state.PeerCertificates[0]

	// valid 对齐 Node socket.authorized：证书链是否通过系统根校验。
	// Go 在 InsecureSkipVerify 下不填 VerifiedChains，需要手动校验补齐
	// （Node 的 rejectUnauthorized:false 时 authorized 同样反映真实校验结果）。
	valid := len(state.VerifiedChains) > 0
	if !valid {
		valid = verifyChainManually(state.PeerCertificates) == nil
	}

	info := map[string]any{
		"certInfo":                buildCertChain(state.PeerCertificates, 0),
		"valid":                   valid,
		"hostnameMatchMonitorUrl": leaf.VerifyHostname(hostname) == nil,
	}
	return info
}

// buildCertChain 把证书链构建成官方嵌套形状（leaf → issuerCertificate → …）。
//
// 官方 parseCertificateInfo 语义：
//   - 链上只有一张 → "self-signed"
//   - i==0 → "server"
//   - 链尾 → "root CA"
//   - 中间 → "intermediate CA"
func buildCertChain(chain []*x509.Certificate, i int) map[string]any {
	cert := chain[i]
	m := certMap(cert)

	switch {
	case len(chain) == 1:
		m["certType"] = "self-signed"
	case i == 0:
		m["certType"] = "server"
	case i == len(chain)-1:
		m["certType"] = "root CA"
	default:
		m["certType"] = "intermediate CA"
	}

	if i+1 < len(chain) {
		m["issuerCertificate"] = buildCertChain(chain, i+1)
	} else {
		m["issuerCertificate"] = nil
	}
	return m
}

// certMap 把一张证书转成 Node getPeerCertificate 的字段形状。
func certMap(cert *x509.Certificate) map[string]any {
	m := map[string]any{
		"subject":        pkixToMap(cert.Subject),
		"issuer":         pkixToMap(cert.Issuer),
		"valid_from":     nodeDate(cert.NotBefore),
		"valid_to":       nodeDate(cert.NotAfter),
		"validTo":        cert.NotAfter.UTC().Format("2006-01-02T15:04:05.000Z"),
		"fingerprint":    sha1Colon(cert.Raw),
		"fingerprint256": sha256Colon(cert.Raw),
		"daysRemaining":  daysRemaining(cert.NotAfter),
	}

	// validFor 对齐官方 subjectaltname 处理：DNS + IP 条目去掉前缀
	sans := append([]string{}, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		sans = append(sans, ip.String())
	}
	m["validFor"] = sans
	return m
}

// pkixToMap 转成 Node 证书 subject 对象形状（前端 formatSubject 消费 CN/O/C）。
func pkixToMap(n pkix.Name) map[string]any {
	m := map[string]any{}
	if n.CommonName != "" {
		m["CN"] = n.CommonName
	}
	if len(n.Organization) > 0 {
		m["O"] = n.Organization[0]
	}
	if len(n.OrganizationalUnit) > 0 {
		m["OU"] = n.OrganizationalUnit[0]
	}
	if len(n.Country) > 0 {
		m["C"] = n.Country[0]
	}
	if len(n.Province) > 0 {
		m["ST"] = n.Province[0]
	}
	if len(n.Locality) > 0 {
		m["L"] = n.Locality[0]
	}
	return m
}

// nodeDate 模仿 Node getPeerCertificate 的日期文本格式（仅存储参考，前端不消费）。
func nodeDate(t time.Time) string {
	months := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	return fmt.Sprintf("%s %2d %02d:%02d:%02d %d GMT",
		months[int(t.Month())-1], t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Year())
}

// daysRemaining 对齐 dayjs.utc(validTo).diff(now, "day") 的截断语义。
func daysRemaining(t time.Time) int {
	d := time.Until(t).Hours() / 24
	return int(d)
}

// colonHex 把摘要转成冒号分隔大写十六进制（Node fingerprint 形态）。
func colonHex(sum []byte) string {
	h := strings.ToUpper(hex.EncodeToString(sum))
	var sb strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(h[i : i+2])
	}
	return sb.String()
}

func sha1Colon(raw []byte) string {
	s := sha1.Sum(raw)
	return colonHex(s[:])
}

func sha256Colon(raw []byte) string {
	s := sha256.Sum256(raw)
	return colonHex(s[:])
}

// verifyChainManually 在 InsecureSkipVerify 场景下补一次真实校验，
// 使 valid 与 Node authorized 的语义一致。
func verifyChainManually(chain []*x509.Certificate) error {
	if len(chain) == 0 {
		return fmt.Errorf("empty chain")
	}
	pool := x509.NewCertPool()
	for i := 1; i < len(chain); i++ {
		pool.AddCert(chain[i])
	}
	_, err := chain[0].Verify(x509.VerifyOptions{
		Intermediates: pool,
		CurrentTime:   time.Now(),
	})
	return err
}

// KnownRootFPs 收集本次握手校验出的系统根证书指纹（sha256 冒号大写），
// 等价官方 monitor.rootCertificates，用于证书到期通知时跳过已知根证书。
func KnownRootFPs(state *tls.ConnectionState) map[string]bool {
	fps := map[string]bool{}
	for _, chain := range state.VerifiedChains {
		if len(chain) == 0 {
			continue
		}
		root := chain[len(chain)-1]
		fps[sha256Colon(root.Raw)] = true
	}
	return fps
}

// HostnameOf 从监控 URL 提取主机名（官方 getUrl()?.hostname）。
func HostnameOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return u.Hostname()
}
