package yunxiao

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrForbiddenAddress 表示接入点解析到了内网 / 保留地址，连接在 connect(2)
// 之前就被拒绝。
var ErrForbiddenAddress = errors.New("接入点解析到内网或保留地址，已拒绝连接")

const (
	maxEndpointLen = 255
	maxHostLen     = 253
	// MaxAllowedHosts 是管理员允许列表的项数上限。
	MaxAllowedHosts = 50
)

// cgnat 是 100.64.0.0/10（运营商级 NAT），netip 没有现成谓词。
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// thisNetwork 是 0.0.0.0/8（「本网络」，部分系统上等价于本机）。
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// isForbiddenIP 报告 a 是否属于不允许出站访问的地址段（§3.7 第 2 条）。
// IPv4-mapped IPv6 先 Unmap 再判，否则 ::ffff:127.0.0.1 会漏过。
func isForbiddenIP(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() ||
		a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() ||
		cgnat.Contains(a) || thisNetwork.Contains(a)
}

// isLocalhostName 报告 h（已小写）是否是 localhost 或其子域。
func isLocalhostName(h string) bool {
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// normalizeHost 小写化并去掉尾点。
func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// ValidateEndpoint 校验用户填写的接入点，返回规范化后的 https://host。
//
// 保存连接时与 NewClient 共用（防线放在使用点）。主防线是精确的 host
// 允许列表：只放行中心版主机与 allowed（管理员登记的 Region 版实例域名）；
// 不支持通配——*.aliyuncs.com 下有 OSS/FC 等用户可控服务。
func ValidateEndpoint(raw string, allowed []string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("接入点不能为空")
	}
	if len(raw) > maxEndpointLen {
		return "", errors.New("接入点过长（上限 255 字符）")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("接入点不是合法 URL")
	}
	if u.Scheme != "https" {
		return "", errors.New("接入点必须使用 https://")
	}
	if u.User != nil {
		return "", errors.New("接入点不能包含用户名或密码")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "?#") {
		return "", errors.New("接入点不能包含查询串或片段")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("接入点只填域名，不要带路径")
	}
	if p := u.Port(); p != "" && p != "443" {
		return "", errors.New("接入点端口只能是 443")
	}
	host := normalizeHost(u.Hostname())
	if host == "" {
		return "", errors.New("接入点缺少主机名")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", errors.New("接入点不能是 IP 地址")
	}
	if isLocalhostName(host) {
		return "", errors.New("接入点不能是 localhost")
	}
	if host != DefaultHost && !containsHost(allowed, host) {
		return "", errors.New("接入点不在允许列表中，Region 版实例域名请联系管理员登记")
	}
	return "https://" + host, nil
}

func containsHost(list []string, host string) bool {
	for _, h := range list {
		if normalizeHost(h) == host {
			return true
		}
	}
	return false
}

// ValidateHostEntry 校验管理员登记的一项允许主机，返回规范化值：
// 合法 DNS 名、小写、无端口、无通配、非 IP 字面量、非 localhost。
func ValidateHostEntry(h string) (string, error) {
	host := normalizeHost(h)
	if host == "" {
		return "", errors.New("域名不能为空")
	}
	if len(host) > maxHostLen {
		return "", fmt.Errorf("域名 %q 过长（上限 253 字符）", host)
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return "", fmt.Errorf("%q 是 IP 地址，只能登记域名", host)
	}
	if isLocalhostName(host) {
		return "", fmt.Errorf("%q 不能登记", host)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%q 不是完整域名", host)
	}
	for _, l := range labels {
		if !validLabel(l) {
			return "", fmt.Errorf("%q 不是合法域名（不支持端口与通配符）", host)
		}
	}
	return host, nil
}

func validLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, r := range l {
		if !isLower(r) && !isDigit(r) && r != '-' {
			return false
		}
	}
	return true
}

func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// lookupFunc 把主机名解析成 IP。只供测试注入；nil 表示用系统解析器。
// 注入点只换「主机名 → IP」，Dialer.Control 检查的是实际拨号地址，
// 注入的解析器绕不过它。
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// controlDial 在 connect(2) 之前检查实际要连的 ip:port。
func controlDial(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w（无法解析拨号地址）", ErrForbiddenAddress)
	}
	if isForbiddenIP(ap.Addr()) {
		return ErrForbiddenAddress
	}
	return nil
}

// guardedTransport 构造出站守卫传输层：拨号期 IP 校验、不走环境代理。
// 只用 stdlib（net、net/netip、syscall）。
func guardedTransport(lookup lookupFunc) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: controlDial}
	dial := d.DialContext
	if lookup != nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := lookup(ctx, host)
			if err != nil {
				return nil, err
			}
			last := ErrForbiddenAddress
			for _, ip := range ips {
				c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		}
	}
	return &http.Transport{
		// 不读 HTTPS_PROXY：走代理时拨号校验校验的是代理 IP 而不是目标。
		Proxy:                 nil,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
	}
}

// refuseRedirect 一律不跟随重定向：允许列表内的主机若能 302 到任意地址，
// 允许列表就形同虚设。返回 ErrUseLastResponse 让调用方拿到 3xx 自行归类。
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// newHTTPClient 包一层统一的超时与重定向策略。
func newHTTPClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Timeout: requestTimeout, CheckRedirect: refuseRedirect}
}
