package config

import "testing"

// AC-12：环境兜底的 GitHub webhook 密钥进入脱敏权威层（KnownSecrets 自称覆盖
// 「集成 token、webhook 密钥」，此前漏了它）。
func TestKnownSecretsIncludesWebhookSecrets(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"GitHub webhook 密钥", Config{GitHubWebhookSecret: "gh-whsec-0123456789"}, "gh-whsec-0123456789"},
		{"Linear webhook 密钥（既有）", Config{LinearWebhookSecret: "lin-whsec-0123456789"}, "lin-whsec-0123456789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.KnownSecrets()
			found := false
			for _, v := range got {
				if v == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("KnownSecrets() = %v，want 含 %q", got, tc.want)
			}
		})
	}
}
