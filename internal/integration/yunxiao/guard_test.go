package yunxiao

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// AC-24
func TestIsForbiddenIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.8.9.10", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		{"169.254.169.254", true},
		{"fe80::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"224.0.0.1", true},
		{"ff02::1", true},
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.1.2.3", true},
		{"8.8.8.8", false},
		{"47.98.1.1", false},
		{"100.128.0.1", false},
		{"172.32.0.1", false},
		{"2408:4000::1", false},
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			a := netip.MustParseAddr(c.ip)
			if got := isForbiddenIP(a); got != c.want {
				t.Errorf("isForbiddenIP(%s) got %v want %v", c.ip, got, c.want)
			}
		})
	}
}

// AC-24 / AC-11 的纯函数部分
func TestValidateEndpoint(t *testing.T) {
	allowed := []string{"devops.cn-hangzhou.aliyuncs.example"}
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"center_default", "https://openapi-rdc.aliyuncs.com", "https://openapi-rdc.aliyuncs.com", true},
		{"center_trailing_slash", "https://openapi-rdc.aliyuncs.com/", "https://openapi-rdc.aliyuncs.com", true},
		{"center_upper_and_dot", "https://OpenAPI-RDC.aliyuncs.com.", "https://openapi-rdc.aliyuncs.com", true},
		{"center_port_443", "https://openapi-rdc.aliyuncs.com:443", "https://openapi-rdc.aliyuncs.com", true},
		{"allowlisted_region", "https://devops.cn-hangzhou.aliyuncs.example", "https://devops.cn-hangzhou.aliyuncs.example", true},
		{"http_scheme", "http://openapi-rdc.aliyuncs.com", "", false},
		{"localhost", "https://localhost", "", false},
		{"sub_localhost", "https://yx.localhost", "", false},
		{"loopback_literal", "https://127.0.0.1", "", false},
		{"private_literal", "https://10.0.0.1", "", false},
		{"ipv6_loopback", "https://[::1]", "", false},
		{"not_allowlisted", "https://evil.example.com", "", false},
		{"wildcard_suffix_not_allowed", "https://oss.aliyuncs.com", "", false},
		{"userinfo", "https://u:p@openapi-rdc.aliyuncs.com", "", false},
		{"port_8443", "https://openapi-rdc.aliyuncs.com:8443", "", false},
		{"query", "https://openapi-rdc.aliyuncs.com/?a=1", "", false},
		{"fragment", "https://openapi-rdc.aliyuncs.com/#x", "", false},
		{"path", "https://openapi-rdc.aliyuncs.com/x", "", false},
		{"empty", "", "", false},
		{"too_long", "https://" + strings.Repeat("a", 250) + ".com", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateEndpoint(c.in, allowed)
			if c.ok {
				if err != nil || got != c.want {
					t.Errorf("ValidateEndpoint(%q) got (%q, %v) want (%q, nil)", c.in, got, err, c.want)
				}
				return
			}
			if err == nil {
				t.Errorf("ValidateEndpoint(%q) got (%q, nil) want error", c.in, got)
			}
		})
	}
}

// guardTarget 起一个 TLS httptest 服务器（监听 127.0.0.1）并数请求。
func guardTarget(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(`{"id":"u"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// guardedClientTo 用守卫传输层造一个 Client，主机名经注入的解析器映射到 ip。
func guardedClientTo(t *testing.T, srv *httptest.Server, host string, ip string) *Client {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	tr := guardedTransport(func(ctx context.Context, h string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	})
	// 让 TLS 校验不成为拒绝的原因：信任 httptest 的证书。守卫本身不看证书。
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tr.TLSClientConfig.InsecureSkipVerify = true
	c := &Client{base: "https://" + host + ":" + u.Port(), token: testToken, hc: newHTTPClient(tr)}
	return c
}

// AC-24
func TestOutboundGuard(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ name, ip string }{
		{"resolved_loopback_refused", "127.0.0.1"},
		{"resolved_private_refused", "10.1.2.3"},
		{"resolved_metadata_refused", "169.254.169.254"},
		{"resolved_mapped_v6_loopback_refused", "::ffff:127.0.0.1"},
		{"resolved_cgnat_refused", "100.64.1.1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, n := guardTarget(t)
			cl := guardedClientTo(t, srv, "intranet.example.test", c.ip)
			_, err := cl.CurrentUser(ctx)
			if err == nil {
				t.Fatal("got nil err, want 拒绝")
			}
			if !errors.Is(err, ErrForbiddenAddress) {
				t.Errorf("错误分类 got %v want ErrForbiddenAddress", err)
			}
			if got := n.Load(); got != 0 {
				t.Errorf("上游收到请求 got %d want 0", got)
			}
		})
	}

	t.Run("system_resolver_localhost_refused", func(t *testing.T) {
		if _, err := ValidateEndpoint("https://yx.localhost", nil); err == nil {
			t.Error("ValidateEndpoint(yx.localhost) got nil want error")
		}
		srv, n := guardTarget(t)
		u, _ := url.Parse(srv.URL)
		tr := guardedTransport(nil)
		tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		cl := &Client{base: "https://yx.localhost:" + u.Port(), token: testToken, hc: newHTTPClient(tr)}
		if _, err := cl.CurrentUser(ctx); err == nil {
			t.Error("got nil err, want 拒绝")
		}
		if got := n.Load(); got != 0 {
			t.Errorf("上游收到请求 got %d want 0", got)
		}
	})

	t.Run("redirect_not_followed", func(t *testing.T) {
		var hits atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		t.Cleanup(target.Close)
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/steal", http.StatusFound)
		}))
		t.Cleanup(src.Close)
		// 生产客户端的 CheckRedirect；传输层用 httptest 的（拨 127.0.0.1 在这里不是被测点）
		cl, err := NewClientWithHTTP(Config{Endpoint: src.URL, Token: testToken}, newHTTPClient(src.Client().Transport), true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cl.CurrentUser(ctx)
		if err == nil || !strings.Contains(err.Error(), "重定向") {
			t.Errorf("got %v want 含「重定向」的错误", err)
		}
		if got := hits.Load(); got != 0 {
			t.Errorf("重定向目标收到请求 got %d want 0", got)
		}
	})

	t.Run("proxy_disabled", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		tr := guardedTransport(nil)
		if tr.Proxy != nil {
			t.Error("Transport.Proxy got 非 nil want nil")
		}
		c, err := NewClient(Config{Endpoint: DefaultEndpoint, Token: testToken})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		ht, ok := c.hc.Transport.(*http.Transport)
		if !ok || ht.Proxy != nil {
			t.Errorf("生产客户端的 Transport.Proxy 应为 nil，got %T", c.hc.Transport)
		}
		if c.hc.CheckRedirect == nil {
			t.Error("生产客户端应设置 CheckRedirect")
		}
	})

	t.Run("new_client_rejects_unlisted_host", func(t *testing.T) {
		if _, err := NewClient(Config{Endpoint: "https://evil.example.com", Token: testToken}); err == nil {
			t.Error("got nil err want error")
		}
		if _, err := NewClient(Config{Endpoint: "https://evil.example.com", Token: testToken, AllowedHosts: []string{"evil.example.com"}}); err != nil {
			t.Errorf("允许列表内: got %v want nil", err)
		}
		if _, err := NewClient(Config{Endpoint: DefaultEndpoint, OrganizationID: "a/b", Token: testToken}); err == nil {
			t.Error("组织 ID 含 /: got nil want error")
		}
		if _, err := NewClient(Config{Endpoint: DefaultEndpoint, Token: " "}); err == nil {
			t.Error("空令牌: got nil want error")
		}
	})
}
