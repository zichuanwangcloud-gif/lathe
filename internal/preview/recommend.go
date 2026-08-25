package preview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
)

// recommendTimeout 是单次 AI 推荐的硬上限。推荐是只读的仓库分析，
// 正常几十秒；给 3 分钟余量，超时杀掉（agent.Driver 自身超时是
// 实现任务的 45 分钟口径，对推荐太宽）。
const recommendTimeout = 3 * time.Minute

// Recommendation 是 AI 对「这个任务该怎么跑预览」的建议。
//
// 定位：建议只是预填 —— 勾选、改值、按下启动的永远是人
// （连不连共享测试库这类有数据风险的决定不交给 AI）。
// 但口令类值不需要人填：复用场景从在跑容器读凭据，其余自动生成。
type Recommendation struct {
	// Path / Kind 是推荐的候选，必须来自候选清单（解析时校验）。
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	// Database 是数据库策略：复用主部署 / 克隆 / 全新 / 不需要。
	Database *DatabaseRecommendation `json:"database,omitempty"`
	// Env 是建议的变量值：变量名 → 值与证据来源。
	Env map[string]EnvSuggestion `json:"env"`
	// Infra 是建议附加的基础设施（仅 Dockerfile 候选有意义）。
	Infra []string `json:"infra"`
	// Notes 是需要人注意的事项（找不到证据的变量、数据风险提示等）。
	Notes string `json:"notes"`
}

// DatabaseRecommendation 是数据库策略建议。
type DatabaseRecommendation struct {
	// Strategy: reuse=连主部署在跑的库（无 SQL 变更时）；clone=克隆一份
	// 随便跑迁移；fresh=全新空库；none=不需要数据库。
	Strategy string `json:"strategy"`
	Source   string `json:"source"`  // reuse/clone 的源容器名
	DBName   string `json:"db_name"` // clone 的源库名（空则取源容器 POSTGRES_DB）
	Reason   string `json:"reason"`
}

// EnvSuggestion 是一条变量建议。Source 必须说明证据来源
// （仓库文件:行 或 本机容器名），人要据此判断信不信。
type EnvSuggestion struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// RecommendOp 是一次推荐操作的状态。
type RecommendOp struct {
	State     string          `json:"state"` // running | done | failed
	Error     string          `json:"error,omitempty"`
	Result    *Recommendation `json:"result,omitempty"`
	StartedAt time.Time       `json:"started_at"`

	head   string // 缓存键：worktree HEAD，代码变了推荐作废
	cancel context.CancelFunc
}

// AgentRunner 是推荐所需的 agent 能力（*agent.Driver 满足）。
// 定义为接口是为了测试可替换 —— 假件喂固定 JSON 验证解析与校验。
type AgentRunner interface {
	Run(ctx context.Context, p agent.RunParams) (*agent.Result, error)
}

// SetRecommender 装配 AI 推荐能力。agent 为 nil 时推荐不可用
// （Recommend 返回 ErrRecommendUnavailable），其余预览功能不受影响。
// channel 是 cc-switch 通道名（B2-2：推荐与分诊同级，走便宜通道）。
func (m *Manager) SetRecommender(ag AgentRunner, channel, settingSources string) {
	m.agent = ag
	m.agentChannel = channel
	m.settingSources = settingSources
}

// ErrRecommendUnavailable 表示未装配 agent（如测试环境）。
var ErrRecommendUnavailable = errors.New("preview: 未配置 agent，AI 推荐不可用")

// Recommend 启动一次异步推荐。同任务同 HEAD 已完成的结果直接复用
// （缓存），不重复烧 agent 调用。
func (m *Manager) Recommend(ctx context.Context, taskID int64, worktree, issueContext string) error {
	if m.agent == nil {
		return ErrRecommendUnavailable
	}
	abs := worktree
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("preview: worktree 不存在: %s", worktree)
	}
	head, _, err := m.exec(ctx, "git", "-C", abs, "rev-parse", "HEAD")
	if err != nil {
		head = "" // 拿不到 HEAD 也能推荐，只是不做缓存命中
	}
	head = strings.TrimSpace(head)

	m.mu.Lock()
	if op := m.recOps[taskID]; op != nil {
		// 进行中：幂等返回；已完成且 HEAD 未变：用缓存
		if op.State == "running" || (op.State == "done" && op.head == head && head != "") {
			m.mu.Unlock()
			return nil
		}
	}
	op := &RecommendOp{State: "running", StartedAt: time.Now(), head: head}
	m.recOps[taskID] = op
	m.mu.Unlock()

	go m.runRecommend(taskID, abs, issueContext, op)
	return nil
}

// RecommendStatus 返回推荐操作状态（从未推荐过返回 nil）。
func (m *Manager) RecommendStatus(taskID int64) *RecommendOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.recOps[taskID]
	if op == nil {
		return nil
	}
	cp := *op
	cp.cancel = nil
	return &cp
}

func (m *Manager) runRecommend(taskID int64, absWorktree, issueContext string, op *RecommendOp) {
	ctx, cancel := context.WithTimeout(context.Background(), recommendTimeout)
	m.mu.Lock()
	op.cancel = cancel
	m.mu.Unlock()
	defer cancel()

	fail := func(err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.recOps[taskID] == op {
			op.State = "failed"
			op.Error = err.Error()
		}
		slog.Warn("预览推荐失败", "task", taskID, "err", err)
	}

	cands, err := Discover(absWorktree)
	if err != nil {
		fail(err)
		return
	}
	if len(cands) == 0 {
		fail(errors.New("worktree 里没有可启动的候选"))
		return
	}

	// 机械证据：改动画像（改了哪个应用、有没有 SQL 变更）与在跑容器
	// 清单（含 DB 凭据）由 Lathe 算好喂给模型 —— 不靠模型即兴发现。
	profile := m.changeProfile(ctx, absWorktree)
	running := m.runningContainers(ctx)

	prompt := recommendPrompt(issueContext, cands, profile, running)
	res, err := m.agent.Run(ctx, agent.RunParams{
		Prompt:         prompt,
		Dir:            absWorktree,
		SessionID:      newUUID(), // claude 要求 UUID 格式（实测非 UUID 直接拒跑）
		PermissionMode: "plan",    // 只读分析，不许改 worktree
		SettingSources: m.settingSources,
		ExtraEnv:       channelEnv(m.agentChannel),
	})
	if err != nil {
		fail(fmt.Errorf("agent 执行失败: %w", err))
		return
	}
	if res.IsError {
		fail(fmt.Errorf("agent 未成功结束（%s）: %s", res.Subtype, tail(res.Text, 300)))
		return
	}

	rec, err := parseRecommendation(res.Text, cands, profile, running)
	if err != nil {
		fail(err)
		return
	}

	m.mu.Lock()
	if m.recOps[taskID] == op {
		op.State = "done"
		op.Result = rec
	}
	m.mu.Unlock()
	slog.Info("预览推荐完成", "task", taskID, "path", rec.Path, "cost", res.CostUSD)
}

// recommendPrompt 组装推荐 prompt。证据全是机械事实：issue 上下文、
// 改动画像（应用 + SQL 标记 + 文件清单）、候选清单、在跑容器（含 DB
// 凭据）。模型读部署文档后拍策略，危险位由解析层机械校验。
func recommendPrompt(issueContext string, cands []Candidate, profile *ChangeProfile, running []RunningContainer) string {
	var b strings.Builder
	b.WriteString(`你在为一个任务预览环境做「怎么跑起来」的推荐。**只读分析：不要修改任何文件，不要构建或启动任何容器。**

## 背景
用户想把这个任务的改动跑成可访问的预览服务，手动验证效果。
**先找并阅读项目的部署文档**（docs/、deploy/ 目录、README 的部署章节），
推荐方案以部署文档为准；文档没覆盖的再自行推断。
`)
	if strings.TrimSpace(issueContext) != "" {
		fmt.Fprintf(&b, "\n## 任务 issue\n%s\n", issueContext)
	}
	if profile != nil && len(profile.Files) > 0 {
		fmt.Fprintf(&b, "\n## 本次改动（相对 %s）\n涉及应用：%s\nSQL/迁移变更：%v\n文件清单：\n```\n%s\n```\n",
			profile.Base, strings.Join(profile.Apps, ", "), profile.HasSQL,
			strings.Join(profile.Files, "\n"))
	}
	b.WriteString("\n## 候选启动单元\n")
	for _, c := range cands {
		fmt.Fprintf(&b, "- [%s] %s", c.Kind, c.Path)
		if c.Kind == "compose" {
			var req []string
			for _, e := range c.Env {
				if e.Required {
					req = append(req, e.Name)
				}
			}
			if len(req) > 0 {
				fmt.Fprintf(&b, "（必填变量: %s）", strings.Join(req, ", "))
			}
		} else if len(c.Ports) > 0 {
			fmt.Fprintf(&b, "（EXPOSE: %v）", c.Ports)
		}
		b.WriteString("\n")
	}
	if len(running) > 0 {
		b.WriteString("\n## 本机正在运行的容器（可复用的依赖服务；DB 类已附凭据）\n```\n")
		for _, c := range running {
			fmt.Fprintf(&b, "%s (%s) %s", c.Name, c.Image, c.Ports)
			if c.DBKind != "" {
				fmt.Fprintf(&b, " [%s]", c.DBKind)
				if c.HostPort > 0 {
					fmt.Fprintf(&b, " 宿主端口=%d", c.HostPort)
				}
				for _, k := range []string{"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB",
					"MYSQL_USER", "MYSQL_PASSWORD", "MYSQL_DATABASE", "MYSQL_ROOT_PASSWORD"} {
					if v := c.Env[k]; v != "" {
						fmt.Fprintf(&b, " %s=%s", k, v)
					}
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("```\n")
	}
	b.WriteString(`
## 要求
1. 哪个候选最适合预览这个 issue 的改动 —— **改动涉及哪个应用就预览哪个**
   （以上改动画像已给出）；同一应用既有 compose 又有 Dockerfile 时优先
   compose（编排自带依赖拓扑）
2. 数据库策略 database.strategy：
   - reuse：主部署的库正在跑且**本次没有 SQL/迁移变更** → 直接连它，
     复用已有测试数据（source 填容器名）
   - clone：有 SQL/迁移变更 → 从 source 容器克隆一份独立库，迁移随便跑
   - fresh：需要数据库但没有可复用的 → 全新空库，应用自己跑迁移
   - none：不需要数据库
   有 SQL 变更时选 reuse 会被系统强制纠正为 clone（迁移可能改坏共享库）。
3. 对 compose 候选：给出每个必填变量的建议值。**只能从仓库内证据
   （.env.example、部署文档、其他 compose 的默认值）或上面在跑服务的
   凭据推断**；口令类变量（PASSWORD/SECRET/TOKEN 等）不用你填也不用
   人填 —— 系统会自动生成或从复用容器读取，把它们留空并在 notes 说明
4. 对 Dockerfile 候选：建议需要哪些附加基础设施（postgres / redis / mysql
   子集，fresh 空库由系统起官方镜像）

请只输出一个 JSON 对象，不要有其他内容：
{
  "path": "推荐的候选路径（必须原样来自候选清单）",
  "kind": "compose 或 dockerfile",
  "reason": "推荐理由，一两句话，引用仓库内证据",
  "database": {"strategy": "reuse|clone|fresh|none", "source": "容器名", "db_name": "源库名", "reason": "一句话"},
  "env": {"变量名": {"value": "建议值", "source": "证据来源，如 apps/x/.env.example:3 或 本机容器 lathe-postgres-dev"}},
  "infra": ["postgres"],
  "notes": "需要人注意的事项，没有则为空字符串"
}`)
	return b.String()
}

// parseRecommendation 解析并校验 agent 输出。
//
// 校验是信任边界：模型可能幻觉出不存在的候选路径或变量名，
// 推荐路径必须在候选清单里、变量名必须在扫描结果里，否则宁可报错
// 让人手工选，也不把幻觉预填进表单。数据库策略的危险位（有 SQL
// 变更选 reuse）在这里机械纠正并留痕，不信模型自觉。
func parseRecommendation(output string, cands []Candidate, profile *ChangeProfile, running []RunningContainer) (*Recommendation, error) {
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("preview: 推荐输出里找不到 JSON 对象: %s", tail(output, 300))
	}
	var rec Recommendation
	if err := json.Unmarshal([]byte(output[start:end+1]), &rec); err != nil {
		return nil, fmt.Errorf("preview: 解析推荐 JSON 失败: %w（原文: %s）", err, tail(output, 300))
	}

	byPath := map[string]Candidate{}
	for _, c := range cands {
		byPath[c.Path] = c
	}
	cand, ok := byPath[rec.Path]
	if !ok {
		return nil, fmt.Errorf("preview: 推荐的路径不在候选清单里: %q", rec.Path)
	}
	wantKind := cand.Kind
	if wantKind == "" {
		wantKind = "dockerfile"
	}
	if rec.Kind != wantKind {
		rec.Kind = wantKind // 以扫描结果为准，模型填错类型不必判死
	}

	// 变量名过滤：只保留该候选扫描出的变量；幻觉出来的名字丢弃
	if len(rec.Env) > 0 && cand.Kind == "compose" {
		known := map[string]bool{}
		for _, e := range cand.Env {
			known[e.Name] = true
		}
		for name := range rec.Env {
			if !known[name] {
				delete(rec.Env, name)
			}
		}
	}
	// infra 过滤：只保留目录里有的
	if len(rec.Infra) > 0 {
		known := map[string]bool{}
		for name := range InfraCatalog {
			known[name] = true
		}
		var kept []string
		for _, name := range rec.Infra {
			if known[name] {
				kept = append(kept, name)
			}
		}
		rec.Infra = kept
	}

	// 数据库策略校验
	if rec.Database != nil {
		if err := validateDatabaseStrategy(rec.Database, profile, running, &rec); err != nil {
			return nil, err
		}
	}
	rec.Reason = strings.TrimSpace(rec.Reason)
	rec.Notes = strings.TrimSpace(rec.Notes)
	return &rec, nil
}

// validateDatabaseStrategy 机械校验数据库策略。纠正动作全部记入
// notes（静默降级可以，静默无痕不行）。
func validateDatabaseStrategy(db *DatabaseRecommendation, profile *ChangeProfile, running []RunningContainer, rec *Recommendation) error {
	switch db.Strategy {
	case "none", "fresh", "":
		if db.Strategy == "" {
			db.Strategy = "none"
		}
		return nil
	case "reuse", "clone":
	default:
		return fmt.Errorf("preview: 未知的数据库策略 %q", db.Strategy)
	}

	// 硬护栏：有 SQL 变更禁止复用共享库 —— 迁移可能改坏别人在用的数据。
	if db.Strategy == "reuse" && profile != nil && profile.HasSQL {
		db.Strategy = "clone"
		appendNote(rec, "检测到 SQL/迁移变更，复用共享库已被系统纠正为克隆一份独立库")
	}

	// 源容器必须真实存在且是 DB 家族
	var src *RunningContainer
	for i := range running {
		if running[i].Name == db.Source {
			src = &running[i]
			break
		}
	}
	if src == nil {
		return fmt.Errorf("preview: 数据库策略的源容器 %q 不在运行中", db.Source)
	}
	if src.DBKind == "" {
		return fmt.Errorf("preview: 源容器 %q（%s）不是数据库镜像", db.Source, src.Image)
	}
	// 克隆本期只支持 postgres（dump|restore 管道）；其他家族降级 fresh 并留痕
	if db.Strategy == "clone" && src.DBKind != "postgres" {
		db.Strategy = "fresh"
		db.Source = ""
		appendNote(rec, fmt.Sprintf("%s 的克隆暂不支持（本期仅 postgres），已改为全新空库", src.DBKind))
	}
	return nil
}

func appendNote(rec *Recommendation, s string) {
	if rec.Notes == "" {
		rec.Notes = s
		return
	}
	rec.Notes += "；" + s
}

// channelEnv 把 cc-switch 通道名编成注入 agent 子进程的环境变量
// （与 runner 包同款，B2-2）；空通道不注入，走 wrapper 默认。
func channelEnv(channel string) []string {
	if strings.TrimSpace(channel) == "" {
		return nil
	}
	return []string{"LATHE_AGENT_CHANNEL=" + strings.TrimSpace(channel)}
}

// newUUID 生成 v4 UUID（claude --session-id 只认这个格式）。
// 与 runner 包同款小 helper，preview 不反向依赖 runner。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("preview: 生成 UUID 失败: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// sortedEnvNames 稳定排序，便于测试与展示。
func sortedEnvNames(env map[string]EnvSuggestion) []string {
	names := make([]string, 0, len(env))
	for n := range env {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
