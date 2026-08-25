package preview

import (
	"context"
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
type Recommendation struct {
	// Path / Kind 是推荐的候选，必须来自候选清单（解析时校验）。
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	// Env 是建议的变量值：变量名 → 值与证据来源。
	Env map[string]EnvSuggestion `json:"env"`
	// Infra 是建议附加的基础设施（仅 Dockerfile 候选有意义）。
	Infra []string `json:"infra"`
	// Notes 是需要人注意的事项（找不到证据的变量、数据风险提示等）。
	Notes string `json:"notes"`
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

	// 本机正在运行的容器作为「可复用依赖」的证据交给 agent
	// （例：本机 55432 已有 postgres 在跑 → DATABASE_HOST 可以指它）
	running, _, _ := m.exec(ctx, "docker", "ps", "--format",
		"{{.Names}}\t{{.Image}}\t{{.Ports}}")

	prompt := recommendPrompt(issueContext, cands, running)
	res, err := m.agent.Run(ctx, agent.RunParams{
		Prompt:         prompt,
		Dir:            absWorktree,
		SessionID:      fmt.Sprintf("preview-recommend-%d-%d", taskID, time.Now().UnixNano()),
		PermissionMode: "plan", // 只读分析，不许改 worktree
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

	rec, err := parseRecommendation(res.Text, cands)
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

// recommendPrompt 组装推荐 prompt。三份证据：issue 上下文（改的是哪个
// 应用）、候选清单（各自的端口与必填变量）、本机在跑的容器（可复用依赖）。
func recommendPrompt(issueContext string, cands []Candidate, runningContainers string) string {
	var b strings.Builder
	b.WriteString(`你在为一个任务预览环境做「怎么跑起来」的推荐。**只读分析：不要修改任何文件，不要构建或启动任何容器。**

## 背景
用户想把这个任务的改动跑成可访问的预览服务，手动验证效果。
`)
	if strings.TrimSpace(issueContext) != "" {
		fmt.Fprintf(&b, "\n## 任务 issue\n%s\n", issueContext)
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
	if strings.TrimSpace(runningContainers) != "" {
		fmt.Fprintf(&b, "\n## 本机正在运行的容器（可能可复用的依赖服务）\n```\n%s```\n", runningContainers)
	}
	b.WriteString(`
## 要求
读仓库（README、deploy 目录、.env.example、compose 文件内容、Dockerfile），判断：
1. 哪个候选最适合预览这个 issue 的改动 —— issue 涉及哪个应用就预览哪个；
   同一应用既有 compose 又有 Dockerfile 时优先 compose（编排自带依赖拓扑）
2. 对 compose 候选：给出每个必填变量的建议值。**只能从仓库内证据
   （.env.example、文档、其他 compose 的默认值）或本机正在运行的服务推断**；
   找不到证据的变量不要编造（尤其口令），在 notes 里说明需要人填
3. 对 Dockerfile 候选：建议需要哪些附加基础设施（postgres / redis / mysql 子集）

请只输出一个 JSON 对象，不要有其他内容：
{
  "path": "推荐的候选路径（必须原样来自候选清单）",
  "kind": "compose 或 dockerfile",
  "reason": "推荐理由，一两句话，引用仓库内证据",
  "env": {"变量名": {"value": "建议值", "source": "证据来源，如 apps/x/.env.example:3 或 本机容器 lathe-postgres-dev"}},
  "infra": ["postgres"],
  "notes": "需要人注意的事项（找不到证据的变量、数据风险提示等），没有则为空字符串"
}`)
	return b.String()
}

// parseRecommendation 解析并校验 agent 输出。
//
// 校验是信任边界：模型可能幻觉出不存在的候选路径或变量名，
// 推荐路径必须在候选清单里、变量名必须在扫描结果里，否则宁可报错
// 让人手工选，也不把幻觉预填进表单。
func parseRecommendation(output string, cands []Candidate) (*Recommendation, error) {
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
	rec.Reason = strings.TrimSpace(rec.Reason)
	rec.Notes = strings.TrimSpace(rec.Notes)
	return &rec, nil
}

// channelEnv 把 cc-switch 通道名编成注入 agent 子进程的环境变量
// （与 runner 包同款，B2-2）；空通道不注入，走 wrapper 默认。
func channelEnv(channel string) []string {
	if strings.TrimSpace(channel) == "" {
		return nil
	}
	return []string{"LATHE_AGENT_CHANNEL=" + strings.TrimSpace(channel)}
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
