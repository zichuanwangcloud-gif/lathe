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
	"github.com/Clouditera/lathe/internal/flow"
	"github.com/Clouditera/lathe/internal/httpapi"
	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/mail"
	"github.com/Clouditera/lathe/internal/preview"
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

// version 是二进制自报的版本号，发布构建用
// -ldflags "-X main.version=<版本>" 注入；本地构建保持 dev。
var version = "dev"

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
			fmt.Println("lathe " + version)
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

	pipeline, previewMgr, err := buildPipeline(cfg, st, factory)
	if err != nil {
		return err
	}

	if ready, missing := factory.ProviderFor(admin.ID).Ready(ctx); !ready {
		slog.Warn("管理员凭据尚不完整，请在设置页配置后再触发任务", "缺少", missing)
	}

	q := newQueue(st, task.NewMachine(st.Pool()), pipeline, factory, cfg)
	// 启动恢复（§6.4 单机形态）：在途任务转回 queued 重派，排队任务重新入队。
	// 必须在 worker 启动前完成，避免同一任务被两边同时捡起。
	if err := q.Reconcile(ctx); err != nil {
		slog.Error("启动恢复失败（继续运行，残留任务可人工重试）", "err", err)
	}

	// 验证隔离栈的开机清扫（T8-AC6）。摆在 worker 启动之前：此刻不可能
	// 有本进程起的栈，全清是安全的；而清扫之后才让 worker 取任务，
	// 就不存在「worker 正要对某个任务起栈，清扫刚把它的栈删了」的窗口。
	// 失败只告警不阻断启动：逐名回收（起栈前 rm -f）还会兜一次。
	if n, nets, err := previewMgr.SweepVerifyStacks(ctx); err != nil {
		slog.Warn("残留验证隔离栈清扫失败（起栈时会按名回收，不影响启动）", "err", err)
	} else if n > 0 || nets > 0 {
		slog.Info("已清扫上一次运行残留的验证隔离栈", "containers", n, "networks", nets)
	}

	// 任务预览环境：在 worktree 里构建镜像、起容器给人手动验证。
	// 阈值现取现用 —— 系统设置里改完即刻生效。
	//
	// AI 推荐：只读 agent 分析仓库后建议「起哪个候选、变量填什么」。
	// 与分诊同级走便宜通道；推荐只是预填，启动仍是人拍板。
	previewMgr.SetRecommender(agent.NewDriver(cfg.ClaudeBin, cfg.AgentTimeout),
		cfg.TriageChannel, cfg.SettingSources)

	// 两条认证通道：邮箱口令（正常登录）与 LATHE_ADMIN_TOKEN 的 Bearer
	// （脚本调用，同时是把自己锁在门外时的应急入口）
	auth := httpapi.NewAuth(os.Getenv("LATHE_ADMIN_TOKEN")).
		WithStore(users, sessions, admin, cfg.SecureCookies())
	auth.TrustProxy = cfg.TrustedProxy
	// 「Linear 任务」菜单的显隐依据：与执行队列同一条凭据通路，
	// 管理员的 env 兜底自然被覆盖；只解密本地凭据，不访问 Linear。
	auth.HasLinearToken = func(ctx context.Context, userID int64) bool {
		_, err := factory.ProviderFor(userID).Linear(ctx)
		return err == nil
	}

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

	// 看板「同步 Linear」的只读接口。凭据与执行队列走同一条通路，
	// 设置页里改完 Linear token 即刻生效，不用重启。
	linearAPI := &httpapi.LinearAPI{
		ClientFor: func(ctx context.Context, userID int64) (*linear.Client, error) {
			return factory.ProviderFor(userID).Linear(ctx)
		},
		Auth: auth,
	}
	linearAPI.Routes(mux)

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
		Store:    st,
	}
	adminAPI.Routes(mux)

	// 任务预览环境：在 worktree 里构建镜像、起容器给人手动验证。
	// 阈值现取现用 —— 系统设置里改完即刻生效。
	//
	// Manager 的构造已经前移进 buildPipeline 了（见那里的注释：验证
	// 隔离栈要在 worker 启动前装好）。这里只接着用。
	//
	// AI 推荐：只读 agent 分析仓库后建议「起哪个候选、变量填什么」。
	// 与分诊同级走便宜通道；推荐只是预填，启动仍是人拍板。
	previewAPI := &httpapi.PreviewAPI{
		Store:    st,
		Auth:     auth,
		Previews: previewMgr,
		IssueContextFor: func(ctx context.Context, userID, taskID int64) string {
			detail, err := st.TaskDetail(ctx, taskID, userID)
			if err != nil {
				return ""
			}
			key := detail.Task.LinearIssueKey
			lin, err := factory.ProviderFor(userID).Linear(ctx)
			if err != nil {
				return key // Linear 未配置：只用 key，推荐质量降级不挡路
			}
			// Linear 的 issue 查询同时接受 UUID 与 identifier
			issue, err := lin.Issue(ctx, key)
			if err != nil {
				return key
			}
			desc := issue.Description
			if len(desc) > 800 {
				desc = desc[:800] + "……"
			}
			return fmt.Sprintf("%s %s\n%s", issue.Identifier, issue.Title, desc)
		},
	}
	previewAPI.Routes(mux)

	// 仓库配置页的「基线目录」检测/部署复用同一个 previewMgr——
	// 检测/部署基线目录跟预览环境是同一套 docker/compose 能力，不必
	// 另起一个 Manager 实例。
	apiSrv.Baselines = previewMgr

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

	// 编排图：一键批量入队（PRD 07 F1.4），与执行队列共用同一个
	// task.Machine，不重开数据库连接。
	flowAPI := &httpapi.FlowAPI{
		Flow: &flow.Service{Pool: st.Pool(), Tasks: task.NewMachine(st.Pool()), Store: st},
		Auth: auth,
	}
	flowAPI.Routes(mux)

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

	// ---- 装配到此为止，下面才启动后台执行者 ----
	//
	// 顺序由代码结构保证，不靠注释提醒：worker 与 merge poller 都在
	// startWorkers 里启动，而那个函数在【所有】装配（含 Pipeline.Stacks、
	// previewMgr、各 API 的依赖）之后才被调用。
	//
	// 为什么这件事必须由结构保证：worker 会读 p.Stacks，而它在 main
	// goroutine 上被赋值 —— 曾经 `go q.work(ctx)` 排在
	// `pipeline.Stacks = ...` 之前，那是货真价实的 data race（interface
	// 值的撕裂读可能读到非 nil 的 itab 配 nil 的 data，在验证主路径上
	// panic；`go test -race` 抓不到，因为 serve() 没有测试覆盖）。
	// 更实的影响：那一段窗口里被领走的任务 p.Stacks == nil → 整个起栈
	// 分支跳过 → 既不起栈也不留痕。而那个窗口恰恰是最热的时刻 ——
	// 上面的 Reconcile 刚把所有在途任务重新入队。
	startWorkers(ctx, q, pipeline, st, factory)

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

// startWorkers 启动后台执行者：任务队列 worker 与合并检测轮询。
//
// 抽成函数是为了让「装配完成后才启动」成为一条【结构性】约束：本函数
// 是唯一的启动点，只要它被放在装配之后调用，就不可能出现「worker 先
// 跑、配置后装」的窗口。调用方见 serve() 的注释。
//
// ctx 是 serve 的根 ctx：SIGINT/SIGTERM 时它被取消，在途任务收到取消
// 而不是被硬杀。注意被取消的 ctx 会一路传进拆栈路径 —— 那正是
// VerifyStack.Down 必须自己解绑取消的原因（见 preview/verifystack.go）。
func startWorkers(ctx context.Context, q *queue, pipeline *runner.Pipeline, st *store.Store, factory runner.ClientFactory) {
	go q.work(ctx)

	// F4.1 合并检测的轮询兜底 + F4.2 现场回收 + F4.3 后继链自动 rebase
	// 跟进 + F2.3-AC2（PR 被关闭未合并 → blocked_dep）的触发源：常驻
	// 轮询 pr_open 任务，见 mergepoll.go。Pipeline 复用 buildPipeline
	// 建出来的那一份——rebase 跟进后的重验（Retry Entry=EntryVerify）
	// 要走跟正常派发完全一样的 stageVerify，两边不能是两套配置。
	//
	// 它同样会起验证栈，所以 B2/B3 的修复（Down 自带解绑取消、起栈前
	// 按名回收）对这条路径自动生效 —— 重验走的就是 stageVerify。
	mergePoller := &runner.MergePoller{
		Tasks:         task.NewMachine(st.Pool()),
		Worktrees:     pipeline.Worktrees,
		ClientFactory: factory,
		Notifier:      pipeline.Notifier,
		RepoLookup:    runner.NewRepoLookup(st.Pool()),
		Pipeline:      pipeline,
		Interval:      45 * time.Second,
	}
	go mergePoller.Run(ctx)
}

// buildPipeline 装配流水线。
//
// 刻意不在此校验 Linear/GitHub 凭据：凭据现在可在界面里配置，
// 缺凭据不该阻止服务启动 —— 否则新用户连配置页都打不开。
// 真正需要凭据时（执行任务）才会报错，并指引去设置页。
//
// 返回 preview.Manager 给调用方复用：验证隔离栈与预览环境用的是同一套
// docker 能力与同一组资源阈值，另起一个实例等于两套配置，迟早不一致
// （apiSrv.Baselines 复用它是同一个先例）。
func buildPipeline(cfg config.Config, st *store.Store, factory runner.ClientFactory) (*runner.Pipeline, *preview.Manager, error) {
	wm, err := runner.NewWorktreeManager(cfg.WorkspaceRoot)
	if err != nil {
		return nil, nil, err
	}

	// Manager 的构造前移到这里（原来是 serve 里 buildPipeline 调用【之后】
	// 才造的，于是 Stacks 只能在更后面赋值）。它本身是纯内存构造，不碰
	// docker，前移无副作用；真正的好处是把「赋值」并进构造：Pipeline
	// 返回时 Stacks 必然已经装好，不存在一个「还没装」的中间态。
	pm := preview.NewManager(cfg.WorkspaceRoot, st.PreviewThresholds)

	return &runner.Pipeline{
		Tasks:            task.NewMachine(st.Pool()),
		Worktrees:        wm,
		Verifier:         runner.NewVerifier(15*time.Minute, cfg.PnpmStore),
		Agent:            agent.NewDriver(cfg.ClaudeBin, cfg.AgentTimeout),
		ClientFactory:    factory,
		Notifier:         logNotifier{},
		Verifications:    st,
		Stacks:           verifyStacks{pm},
		AgentEvents:      st,
		Gates:            runner.NewVerifyGates(cfg.LightSlots, cfg.HeavySlots),
		PermissionMode:   "acceptEdits",
		MaxFixAttempts:   cfg.FixAttempts,
		SettingSources:   cfg.SettingSources,
		TriageChannel:    cfg.TriageChannel,
		ImplementChannel: cfg.ImplementChannel,
	}, pm, nil
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
				"triageChannel": cfg.TriageChannel,
				"implChannel":   cfg.ImplementChannel,
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

// verifyStacks 把 preview.Manager 适配成 runner.VerifyStackUp。
//
// 需要这层薄适配是因为 runner 的窄接口用自己的 VerifyStackHandle
// （runner 不该 import preview 的具体类型），而 preview 返回的是
// *preview.VerifyStack。适配器只做类型搬运与错误身份翻译。
type verifyStacks struct{ m *preview.Manager }

func (v verifyStacks) UpVerifyStack(ctx context.Context, taskID int64, infra []string) (runner.VerifyStackHandle, error) {
	st, err := v.m.UpVerifyStack(ctx, taskID, infra)
	if err != nil {
		// 错误身份翻译：把 preview 的水位错误翻成 runner 认得的那一个，
		// 让 runner 侧的 stackUnavailable 判定生效（决定降级还是判死）。
		//
		// docker 不可用必须**先**判、且翻成另一个身份：CheckResources
		// 的 switch 第一分支就是 !DockerOK → Allowed=false，若它跟水位
		// 共用同一个身份，runner 就会降级、日志写「资源水位不允许」——
		// 把人引去调阈值，而真正该做的是把 docker 起来。
		if errors.Is(err, preview.ErrDockerUnavailableVerify) {
			return nil, fmt.Errorf("%w: %v", runner.ErrStackDockerDown, err)
		}
		if errors.Is(err, preview.ErrVerifyStackOverThreshold) {
			return nil, fmt.Errorf("%w: %v", runner.ErrStackOverThreshold, err)
		}
		return nil, err
	}
	if st == nil {
		// 没声明依赖：返回 nil handle 而不是包着 nil 的接口值 ——
		// 后者会让调用方的 `stack != nil` 判断意外为真，然后对
		// nil 指针调方法。
		return nil, nil
	}
	return verifyStackHandle{st}, nil
}

// verifyStackHandle 适配 *preview.VerifyStack 到 runner.VerifyStackHandle。
//
// StackEnv 从字段变方法：runner 的接口用方法是为了让测试能造假件，
// 而 preview 侧用字段更直白 —— 两边各自合理，这里搬一下。
type verifyStackHandle struct{ st *preview.VerifyStack }

func (h verifyStackHandle) StackEnv() map[string]string    { return h.st.Env }
func (h verifyStackHandle) Down(ctx context.Context) error { return h.st.Down(ctx) }
