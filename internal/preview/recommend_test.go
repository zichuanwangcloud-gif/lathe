package preview

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
)

// fakeAgent 喂固定输出并记录调用次数与 prompt。
type fakeAgent struct {
	mu     sync.Mutex
	output string
	err    error
	calls  int
	prompt string
}

func (f *fakeAgent) Run(ctx context.Context, p agent.RunParams) (*agent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.prompt = p.Prompt
	if f.err != nil {
		return nil, f.err
	}
	return &agent.Result{Text: f.output, Success: true}, nil
}

func waitRecommend(t *testing.T, m *Manager, taskID int64) *RecommendOp {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op := m.RecommendStatus(taskID)
		if op == nil || op.State != "running" {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("推荐未在 5 秒内收尾")
	return nil
}

func TestRecommendHappyPath(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"rev-parse": {stdout: "abc123\n"},
		"ps":        {stdout: "lathe-postgres-dev\tpostgres:18-alpine\t0.0.0.0:55432->5432/tcp\n"},
	}}
	m, wt := newTestManager(t, fd, 100, 100)
	writeFile(t, wt, "deploy/docker-compose.yml",
		"services:\n  web:\n    image: app\n    environment:\n      - DB=${DATABASE_HOST:?必填}\n")

	fa := &fakeAgent{output: `{
	  "path": "deploy/docker-compose.yml",
	  "kind": "compose",
	  "reason": "本单改 console，standalone 编排自带依赖",
	  "env": {"DATABASE_HOST": {"value": "172.17.0.1", "source": "本机容器 lathe-postgres-dev"},
	          "HALLUCINATED": {"value": "x", "source": "编造的"}},
	  "infra": ["postgres", "oracle"],
	  "notes": "口令需人填"
	}`}
	m.SetRecommender(fa, "stg", "project")

	if err := m.Recommend(context.Background(), 7, wt, "CR-1 控制台改版"); err != nil {
		t.Fatal(err)
	}
	op := waitRecommend(t, m, 7)
	if op.State != "done" {
		t.Fatalf("应 done，得到 %+v", op)
	}
	rec := op.Result
	if rec.Path != "deploy/docker-compose.yml" || rec.Kind != "compose" {
		t.Errorf("推荐候选不符: %+v", rec)
	}
	// 幻觉变量被过滤，合法变量保留
	if _, ok := rec.Env["HALLUCINATED"]; ok {
		t.Error("幻觉变量应被过滤")
	}
	if rec.Env["DATABASE_HOST"].Value != "172.17.0.1" {
		t.Errorf("合法变量应保留: %+v", rec.Env)
	}
	// infra 过滤：oracle 不在目录里
	if len(rec.Infra) != 1 || rec.Infra[0] != "postgres" {
		t.Errorf("infra 应只保留已知项: %v", rec.Infra)
	}

	// prompt 应带三份证据：issue、候选（含必填变量）、本机容器
	if !strings.Contains(fa.prompt, "CR-1 控制台改版") ||
		!strings.Contains(fa.prompt, "DATABASE_HOST") ||
		!strings.Contains(fa.prompt, "lathe-postgres-dev") {
		t.Errorf("prompt 缺证据:\n%s", fa.prompt)
	}

	// 同 HEAD 再调一次：命中缓存，不重复调 agent
	if err := m.Recommend(context.Background(), 7, wt, "CR-1 控制台改版"); err != nil {
		t.Fatal(err)
	}
	if fa.calls != 1 {
		t.Errorf("同 HEAD 应命中缓存，agent 调用 %d 次", fa.calls)
	}
}

func TestRecommendValidation(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, wt := newTestManager(t, fd, 100, 100)

	// 未装配 agent → 不可用
	if err := m.Recommend(context.Background(), 7, wt, ""); err == nil {
		t.Error("未装配 agent 应报错")
	}

	// 推荐路径不在候选清单 → failed（幻觉不能进表单）
	fa := &fakeAgent{output: `{"path": "不存在.yml", "kind": "compose", "reason": "x"}`}
	m.SetRecommender(fa, "", "")
	if err := m.Recommend(context.Background(), 7, wt, ""); err != nil {
		t.Fatal(err)
	}
	op := waitRecommend(t, m, 7)
	if op.State != "failed" || !strings.Contains(op.Error, "不在候选清单") {
		t.Errorf("幻觉路径应失败，得到 %+v", op)
	}
}

// agent 在 JSON 前后附带说明文字时也能解析（与分诊同款容忍）。
func TestParseRecommendationTolerant(t *testing.T) {
	cands := []Candidate{{Path: "Dockerfile", Kind: "", Ports: []int{3000}}}
	rec, err := parseRecommendation("好的，分析如下：\n{\"path\": \"Dockerfile\", \"kind\": \"wrong\", \"reason\": \"r\", \"infra\": [], \"notes\": \"\"}\n以上。", cands)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "dockerfile" {
		t.Errorf("kind 应以扫描结果为准纠正为 dockerfile，得到 %s", rec.Kind)
	}
}
