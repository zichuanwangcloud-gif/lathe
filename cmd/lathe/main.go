// Command lathe 是 Lathe 的控制面：HTTP API、任务状态机、串行执行队列。
//
// 当前形态：单机、多用户、按人隔离 —— 每个用户有专属 webhook 地址、
// 各自的任务 / 仓库 / 凭据，队列按属主解析凭据执行。多节点横向扩展
// 见 docs/02-design.md §8。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Clouditera/lathe/internal/auth"
	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/creds"
	"github.com/Clouditera/lathe/internal/httpapi"
	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/mail"
	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/secret"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/Clouditera/lathe/internal/webui"
)

func main() {
	if err := run(); err != nil {
		slog.Error("控制面退出", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			return runMigrate(cfg, os.Args[2:])
		case "version":
			fmt.Println("lathe dev")
			return nil
		case "serve":
			// 显式 serve 与默认行为一致
		default:
			return fmt.Errorf("未知子命令 %q（可用：serve, migrate, version）", os.Args[1])
		}
	}
	return serve(cfg)
}

func runMigrate(cfg config.Config, args []string) error {
	dir := "up"
	if len(args) > 0 {
		dir = args[0]
	}
	if dir != "up" && dir != "down" {
		return fmt.Errorf("migrate 方向须为 up 或 down，得到 %q", dir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	st, err := store.Open(ctx, cfg.Database.DSN())
	if err != nil {
		return err
	}
	defer st.Close()

	if dir == "up" {
		return st.MigrateUp(ctx)
	}
	return st.MigrateDown(ctx)
}

func serve(cfg config.Config) error {
	// 收到 TERM/INT 时让在途任务收到 context 取消，而不是被硬杀
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("Lathe 控制面启动", "config", cfg.Redacted())

	st, err := store.Open(ctx, cfg.Database.DSN())
	if err != nil {
		return err
	}
	defer st.Close()

	// 凭据加密存储：主密钥在数据库之外，拿到库转储也解不出凭据
	key, keySource, err := secret.LoadKey(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return err
	}
	sealer, err := secret.New(key)
	if err != nil {
		return err
	}
	slog.Info("凭据加密已就绪", "key_source", keySource)

	secrets := st.NewSecrets(sealer)
	users := st.NewUsers()
	sessions := st.NewSessions()

	admin, err := ensureSuperadmin(ctx, users, cfg)
	if err != nil {
		return err
	}

	go gcSessions(ctx, sessions)

	// P1.5 第二步：凭据按用户隔离。环境变量兜底只给内置管理员 ——
	// 那是部署者自己的账号，不该借给普通成员的任务用。
	factory := creds.NewFactory(secrets, creds.EnvFallback{
		LinearToken:         cfg.LinearToken,
		LinearWebhookSecret: cfg.LinearWebhookSecret,
		GitHubToken:         cfg.GitHubToken,
		LinearUserID:        os.Getenv("LATHE_LINEAR_USER_ID"),
	}, admin.ID)

	pipeline, err := buildPipeline(cfg, st, factory)
	if err != nil {
		return err
	}

	if ready, missing := factory.ProviderFor(admin.ID).Ready(ctx); !ready {
		slog.Warn("管理员凭据尚不完整，请在设置页配置后再触发任务", "缺少", missing)
	}

	q := newQueue(st, task.NewMachine(st.Pool()), pipeline, factory, cfg)
	go q.work(ctx)

	// 两条认证通道：邮箱口令（正常登录）与 LATHE_ADMIN_TOKEN 的 Bearer
	// （脚本调用，同时是把自己锁在门外时的应急入口）
	auth := httpapi.NewAuth(os.Getenv("LATHE_ADMIN_TOKEN")).
		WithStore(users, sessions, admin, cfg.SecureCookies())
	auth.TrustProxy = cfg.TrustedProxy

	if cfg.BaseURL == "" {
		slog.Warn("未设置 LATHE_BASE_URL，密码重置邮件里的链接将指向本机地址，外网用户点不开",
			"链接前缀", cfg.PublicURL())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpapi.Health)
	// 每用户专属回调：/webhooks/linear/{slug}（设置页展示完整地址）。
	// 旧路径保留，路由到内置管理员，老部署的 Linear webhook 配置不用改。
	webhook := &httpapi.LinearWebhook{
		Resolver:   &webhookResolver{users: users, factory: factory, admin: admin},
		Deliveries: st,
		Tasks:      q,
	}
	mux.Handle("POST /webhooks/linear/{slug}", webhook)
	mux.Handle("POST /webhooks/linear", webhook)

	apiSrv := &httpapi.API{
		Store:        st,
		Tasks:        task.NewMachine(st.Pool()),
		Queue:        q,
		Auth:         auth,
		ConfigStatus: configStatus(cfg),
	}
	apiSrv.Routes(mux)

	accountAPI := &httpapi.AccountAPI{
		Users:      users,
		Sessions:   sessions,
		Resets:     st.NewResets(),
		Auth:       auth,
		Mail:       mail.NewSender(secrets.LoadSMTP),
		BaseURL:    cfg.PublicURL(),
		TrustProxy: cfg.TrustedProxy,
	}
	accountAPI.Routes(mux)

	smtpAPI := &httpapi.SMTPAPI{
		Secrets:  secrets,
		Verifier: mail.Verifier{},
		Auth:     auth,
	}
	smtpAPI.Routes(mux)

	adminAPI := &httpapi.AdminAPI{
		Users:    users,
		Sessions: sessions,
		Resets:   st.NewResets(),
		Auth:     auth,
	}
	adminAPI.Routes(mux)

	credAPI := &httpapi.CredentialAPI{
		Secrets:  secrets,
		Verifier: creds.Verifier{},
		Auth:     auth,
		OnChange: factory.Invalidate,
		EnvConfigured: func(kind string) bool {
			switch kind {
			case store.KindLinear:
				return cfg.LinearToken != ""
			case store.KindLinearWebhook:
				return cfg.LinearWebhookSecret != ""
			case store.KindGitHub:
				return cfg.GitHubToken != ""
			}
			return false
		},
	}
	credAPI.Routes(mux)

	if webui.Available() {
		mux.Handle("/", webui.Handler())
	} else {
		slog.Warn("管理界面未构建进二进制，执行 make ui 后重新编译")
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "管理界面未构建：请执行 make ui && make build", http.StatusNotImplemented)
		})
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP 服务监听中", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("收到关闭信号，正在停止")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// buildPipeline 装配流水线。
//
// 刻意不在此校验 Linear/GitHub 凭据：凭据现在可在界面里配置，
// 缺凭据不该阻止服务启动 —— 否则新用户连配置页都打不开。
// 真正需要凭据时（执行任务）才会报错，并指引去设置页。
func buildPipeline(cfg config.Config, st *store.Store, factory runner.ClientFactory) (*runner.Pipeline, error) {
	wm, err := runner.NewWorktreeManager(cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	return &runner.Pipeline{
		Tasks:          task.NewMachine(st.Pool()),
		Worktrees:      wm,
		Verifier:       runner.NewVerifier(15*time.Minute, cfg.PnpmStore),
		Agent:          agent.NewDriver(cfg.ClaudeBin, cfg.AgentTimeout),
		ClientFactory:  factory,
		Notifier:       logNotifier{},
		Verifications:  st,
		Gates:          runner.NewVerifyGates(cfg.LightSlots, cfg.HeavySlots),
		PermissionMode: "acceptEdits",
		SettingSources: cfg.SettingSources,
	}, nil
}

// webhookResolver 把回调路径里的 slug 解析成投递目标。
//
// 空 slug（旧路径）映射到内置管理员 —— 老部署的 webhook 不迁移也能用。
// 签名密钥与接单判定的 Linear 用户 ID 都按目标用户的凭据现取，
// 在设置页改完即刻生效。
type webhookResolver struct {
	users   *store.Users
	factory *creds.Factory
	admin   *store.User
}

func (r *webhookResolver) Resolve(ctx context.Context, slug string) (*httpapi.WebhookTarget, error) {
	u := r.admin
	if slug != "" {
		var err error
		u, err = r.users.ByWebhookSlug(ctx, slug)
		if err != nil {
			return nil, err
		}
	}
	if u.Disabled() {
		return nil, fmt.Errorf("账号已停用")
	}

	p := r.factory.ProviderFor(u.ID)
	secret := p.WebhookSecret(ctx)
	if secret == "" {
		return nil, fmt.Errorf("该用户尚未配置 webhook 签名密钥")
	}
	return &httpapi.WebhookTarget{
		OwnerID:      u.ID,
		Secret:       secret,
		LinearUserID: p.LinearUserID(ctx),
	}, nil
}

// configStatus 生成给管理界面的配置状态。
//
// 只报告「配没配、从哪个环境变量读」，绝不包含凭据内容本身。
func configStatus(cfg config.Config) func() map[string]any {
	return func() map[string]any {
		item := func(v string) map[string]any {
			return map[string]any{"configured": v != ""}
		}
		return map[string]any{
			"linear":        item(cfg.LinearToken),
			"linearWebhook": item(cfg.LinearWebhookSecret),
			"linearUser":    item(os.Getenv("LATHE_LINEAR_USER_ID")),
			"github":        item(cfg.GitHubToken),
			"runtime": map[string]any{
				"node":          cfg.NodeName,
				"workspaceRoot": cfg.WorkspaceRoot,
				"pnpmStore":     cfg.PnpmStore,
				"claudeBin":     cfg.ClaudeBin,
				"agentTimeout":  cfg.AgentTimeout.String(),
				"mode":          "单机多用户（按人隔离）",
			},
		}
	}
}

// logNotifier 是 P0 的占位通知实现：先写日志。
// 真正的推送通道（终端/手机）留到 P2 随 Web UI 一起做。
type logNotifier struct{}

func (logNotifier) Notify(ctx context.Context, msg string) error {
	slog.Warn("【通知】" + msg)
	return nil
}

// ensureSuperadmin 取得（或创建）内置超级管理员。
//
// 口令刻意不在迁移里播种：SQL 算不出 bcrypt，而把一个已知明文的哈希写死在
// 迁移文件里，等于把默认口令发布到公开仓库 —— must_change_password 拦不住
// 「抢在管理员第一次登录之前登进来」，那恰恰是服务刚起、没人盯着的窗口。
//
// 每次启动都跑，幂等：顺带把被误停用的超管救回来，这正是「内置账号」的意义。
func ensureSuperadmin(ctx context.Context, users *store.Users, cfg config.Config) (*store.User, error) {
	u, err := users.EnsureAdmin(ctx, cfg.AdminEmail)
	if err != nil {
		return nil, err
	}
	if u.PasswordHash != "" {
		return u, nil
	}

	// 没有口令 —— 全新安装，或从 P0 升级上来的那条老记录
	pw, generated := cfg.AdminPassword, false
	if pw == "" {
		pw, generated = auth.RandomPassword(), true
	}
	hash, err := auth.Hash(pw)
	if err != nil {
		return nil, fmt.Errorf("生成管理员口令失败: %w", err)
	}
	if err := users.SetPassword(ctx, u.ID, hash, true); err != nil {
		return nil, err
	}

	if generated {
		// 只在自动生成时打印。管理员自己用 LATHE_ADMIN_PASSWORD 指定的口令
		// 不该再被日志复述一遍 —— 日志往往会被收集转发。
		slog.Warn("已为内置管理员生成初始口令，请立即登录并修改（此口令只显示这一次）",
			"email", u.Email, "password", pw)
	} else {
		slog.Info("已用 LATHE_ADMIN_PASSWORD 设置内置管理员的初始口令", "email", u.Email)
	}

	return users.ByID(ctx, u.ID)
}

// gcSessions 定期清理过期会话与过期的密码重置令牌。
func gcSessions(ctx context.Context, sessions *store.Sessions) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := sessions.GC(ctx)
			if err != nil {
				slog.Warn("清理过期会话失败", "err", err)
				continue
			}
			if n > 0 {
				slog.Info("已清理过期会话与重置令牌", "rows", n)
			}
		}
	}
}
