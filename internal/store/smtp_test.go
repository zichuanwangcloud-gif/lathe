package store

import (
	"context"
	"errors"
	"testing"

	"github.com/Clouditera/lathe/internal/secret"
)

func smtpTestStore(t *testing.T) *Secrets {
	t.Helper()

	st := testStore(t)
	key := make([]byte, secret.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}

	// 单行表，测试之间会互相看到彼此的写入，跑完清干净
	secrets := st.NewSecrets(sealer)
	t.Cleanup(func() { _ = secrets.DeleteSMTP(context.Background()) })
	return secrets
}

func sampleSMTP() SMTPConfig {
	return SMTPConfig{
		Host: "smtp.example.com", Port: 587, Username: "user@example.com",
		FromAddr: "lathe@example.com", FromName: "Lathe", TLSMode: TLSStartTLS,
	}
}

func TestSMTPSaveLoadRoundTrip(t *testing.T) {
	s := smtpTestStore(t)
	ctx := context.Background()

	if err := s.SaveSMTP(ctx, sampleSMTP(), "smtp-password"); err != nil {
		t.Fatal(err)
	}
	cfg, pw, err := s.LoadSMTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "smtp.example.com" || cfg.Port != 587 || cfg.TLSMode != TLSStartTLS {
		t.Fatalf("配置未原样返回: %+v", cfg)
	}
	if pw != "smtp-password" {
		t.Fatalf("密码应能解回，得到 %q", pw)
	}
}

// 密码留空表示「不修改」—— 管理员改个端口不该被迫重填密码。
func TestSMTPEmptyPasswordKeepsExisting(t *testing.T) {
	s := smtpTestStore(t)
	ctx := context.Background()

	if err := s.SaveSMTP(ctx, sampleSMTP(), "original-password"); err != nil {
		t.Fatal(err)
	}
	cfg := sampleSMTP()
	cfg.Port = 465
	cfg.TLSMode = TLSImplicit
	if err := s.SaveSMTP(ctx, cfg, ""); err != nil {
		t.Fatal(err)
	}

	got, pw, err := s.LoadSMTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 465 {
		t.Errorf("端口应已更新，得到 %d", got.Port)
	}
	if pw != "original-password" {
		t.Fatalf("密码应保留原值，得到 %q", pw)
	}
}

// 配置变了，上一次的验证结论就不再有效 —— 与 Secrets.Save 同样的取舍。
func TestSMTPSaveClearsVerification(t *testing.T) {
	s := smtpTestStore(t)
	ctx := context.Background()

	if err := s.SaveSMTP(ctx, sampleSMTP(), "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSMTPVerified(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.SMTPStatus(ctx); st.VerifiedAt == nil {
		t.Fatal("标记后应有验证时间")
	}

	cfg := sampleSMTP()
	cfg.Host = "another.example.com"
	if err := s.SaveSMTP(ctx, cfg, ""); err != nil {
		t.Fatal(err)
	}
	st, err := s.SMTPStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.VerifiedAt != nil {
		t.Fatal("改了配置之后不该还显示「已验证」")
	}
}

// 面向界面的状态里绝不能出现明文密码。
func TestSMTPStatusNeverExposesPassword(t *testing.T) {
	s := smtpTestStore(t)
	ctx := context.Background()

	if err := s.SaveSMTP(ctx, sampleSMTP(), "super-secret-value"); err != nil {
		t.Fatal(err)
	}
	st, err := s.SMTPStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.PasswordSet {
		t.Fatal("应报告密码已设置")
	}
	if st.PasswordMasked == "super-secret-value" {
		t.Fatal("状态里出现了明文密码")
	}
	if st.PasswordMasked == "" {
		t.Fatal("应给出掩码供人确认配的是哪个")
	}
}

func TestSMTPStatusWhenUnconfigured(t *testing.T) {
	s := smtpTestStore(t)
	ctx := context.Background()
	_ = s.DeleteSMTP(ctx)

	st, err := s.SMTPStatus(ctx)
	if err != nil {
		t.Fatalf("未配置不该报错，界面要能渲染一张空卡片: %v", err)
	}
	if st.Configured {
		t.Fatal("未配置时 Configured 应为 false")
	}
	// 给界面一个能直接用的默认值，省得每个前端各写一份
	if st.Port != 587 || st.TLSMode != TLSStartTLS {
		t.Errorf("应返回可用的默认值，得到 port=%d tls=%s", st.Port, st.TLSMode)
	}
}

func TestSMTPLoadWhenUnconfigured(t *testing.T) {
	s := smtpTestStore(t)
	ctx := context.Background()
	_ = s.DeleteSMTP(ctx)

	if _, _, err := s.LoadSMTP(ctx); !errors.Is(err, ErrSMTPNotConfigured) {
		t.Fatalf("应返回 ErrSMTPNotConfigured，得到 %v", err)
	}
}

func TestSMTPRejectsInvalidConfig(t *testing.T) {
	s := smtpTestStore(t)
	ctx := context.Background()

	cases := map[string]SMTPConfig{
		"缺主机":     {Port: 587, FromAddr: "a@b.co", TLSMode: TLSStartTLS},
		"端口越界":    {Host: "h", Port: 70000, FromAddr: "a@b.co", TLSMode: TLSStartTLS},
		"缺发件地址":   {Host: "h", Port: 587, TLSMode: TLSStartTLS},
		"加密方式不合法": {Host: "h", Port: 587, FromAddr: "a@b.co", TLSMode: "wat"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.SaveSMTP(ctx, cfg, "pw"); err == nil {
				t.Fatal("应当被拒绝")
			}
		})
	}
}
