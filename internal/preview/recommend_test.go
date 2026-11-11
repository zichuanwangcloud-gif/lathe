package preview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// fakeAgent 喂固定输出并记录调用次数与 prompt。
type fakeAgent struct {
	mu     sync.Mutex
	output string
	err    error
	// res 非 nil 时原样返回（f03：模拟 CLI 报失败、带费用的结果）；默认行为不变。
	res    *agent.Result
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
	if f.res != nil {
		return f.res, nil
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
	rec, err := parseRecommendation("好的，分析如下：\n{\"path\": \"Dockerfile\", \"kind\": \"wrong\", \"reason\": \"r\", \"infra\": [], \"notes\": \"\"}\n以上。", cands, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "dockerfile" {
		t.Errorf("kind 应以扫描结果为准纠正为 dockerfile，得到 %s", rec.Kind)
	}
}

// ---------------------------------------------------------------- f03 用量账本

type fakeUsage struct {
	mu   sync.Mutex
	recs []agent.RunRecord
	err  error
}

func (f *fakeUsage) RecordAgentRun(_ context.Context, r agent.RunRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
	return f.err
}

func (f *fakeUsage) snapshot() []agent.RunRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.RunRecord(nil), f.recs...)
}

const validRec = `{"path":"deploy/docker-compose.yml","kind":"compose","reason":"自带依赖","env":{},"infra":[],"notes":""}`

func recommendFixture(t *testing.T, withCandidate bool) (*Manager, string) {
	t.Helper()
	fd := &fakeDocker{outputs: map[string]fakeResult{"rev-parse": {stdout: "abc123\n"}, "ps": {stdout: ""}}}
	m, wt := newTestManager(t, fd, 100, 100)
	if withCandidate {
		writeFile(t, wt, "deploy/docker-compose.yml", "services:\n  web:\n    image: app\n")
	} else if err := os.Remove(filepath.Join(wt, "Dockerfile")); err != nil {
		// newTestManager 默认放了一个 Dockerfile 候选；「没有候选」要把它拿掉
		t.Fatal(err)
	}
	return m, wt
}

// AC-2d：预览推荐成功与失败都记一行；没起 agent 就没有花费、不记。
func TestRecommendRecordsUsage(t *testing.T) {
	cases := []struct {
		name       string
		candidate  bool
		ag         *fakeAgent
		wantRecs   int
		wantStatus string
		wantNilRes bool
		wantCost   float64
		wantState  string
	}{
		{"成功", true, &fakeAgent{res: &agent.Result{Success: true, Subtype: "success", Text: validRec, CostUSD: 0.05, CostKnown: true}}, 1, "success", false, 0.05, "done"},
		{"agent 报错", true, &fakeAgent{err: errors.New("boom")}, 1, "error", true, 0, "failed"},
		{"CLI 报失败", true, &fakeAgent{res: &agent.Result{IsError: true, Subtype: "error_during_execution", CostUSD: 0.02, CostKnown: true}}, 1, "error", false, 0.02, "failed"},
		{"没有候选：不调 agent", false, &fakeAgent{output: validRec}, 0, "", false, 0, "failed"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, wt := recommendFixture(t, tc.candidate)
			u := &fakeUsage{}
			m.Usage = u
			m.SetRecommender(tc.ag, "", "project")
			taskID := int64(100 + i)
			if err := m.Recommend(context.Background(), taskID, wt, "CR-1"); err != nil {
				t.Fatal(err)
			}
			op := waitRecommend(t, m, taskID)
			if op.State != tc.wantState {
				t.Errorf("State got %s, want %s（%+v）", op.State, tc.wantState, op)
			}
			recs := u.snapshot()
			if len(recs) != tc.wantRecs {
				t.Fatalf("记录数 got %d, want %d", len(recs), tc.wantRecs)
			}
			if tc.wantRecs == 0 {
				return
			}
			r := recs[0]
			if r.Kind != "preview" || r.Phase != "recommend" || r.TaskID != taskID || r.PRDID != 0 {
				t.Errorf("Kind/Phase/TaskID got %s/%s/%d, want preview/recommend/%d", r.Kind, r.Phase, r.TaskID, taskID)
			}
			if got := store.RunStatus(r.Result, r.Err); got != tc.wantStatus {
				t.Errorf("status got %s, want %s", got, tc.wantStatus)
			}
			if (r.Result == nil) != tc.wantNilRes {
				t.Errorf("Result==nil got %v, want %v", r.Result == nil, tc.wantNilRes)
			}
			if r.Result != nil && r.Result.CostUSD != tc.wantCost {
				t.Errorf("CostUSD got %v, want %v", r.Result.CostUSD, tc.wantCost)
			}
			if r.StartedAt.IsZero() || r.EndedAt.Before(r.StartedAt) {
				t.Errorf("StartedAt/EndedAt got %v/%v", r.StartedAt, r.EndedAt)
			}
		})
	}
}

// AC-3：记账失败不影响推荐结果。
func TestRecommendUsageFailureIsNonFatal(t *testing.T) {
	m, wt := recommendFixture(t, true)
	m.Usage = &fakeUsage{err: errors.New("boom")}
	m.SetRecommender(&fakeAgent{output: validRec}, "", "project")
	if err := m.Recommend(context.Background(), 7, wt, "CR-1"); err != nil {
		t.Fatal(err)
	}
	if op := waitRecommend(t, m, 7); op.State != "done" {
		t.Errorf("State got %s, want done（%+v）", op.State, op)
	}
}
