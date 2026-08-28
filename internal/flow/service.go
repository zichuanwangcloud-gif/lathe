package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// ErrIssueActive 表示批次里某个 issue 已经有一个"活着"的任务
// （state 不在 merged/failed/cancelled），因此不能再为它建新任务。
//
// 这正是 task.Machine.Create 撞到 tasks_one_active_per_issue 唯一索引
// 时的语义翻译，也是 F1.1-AC3（同一 issue 不能出现在两个未终结图里）
// 要的行为——只是把裸的 SQL 唯一约束冲突换成人能看懂的错误。
type ErrIssueActive struct {
	IssueKey string
}

func (e ErrIssueActive) Error() string {
	return fmt.Sprintf("issue %s 已经有一个进行中的任务", e.IssueKey)
}

// ErrFlowNotFound 表示编排图不存在，或不属于当前用户
// （两者刻意不分，与 store.ErrRepoNotFound 同一原则）。
var ErrFlowNotFound = errors.New("flow: 编排图不存在")

// NodeInput 是建图请求里的一个节点。
type NodeInput struct {
	// IssueKey 是人看的 issue 编号（如 ENG-123）；IssueID 是 Linear
	// 的 issue UUID。两者至少要有一个非空，缺一个时用另一个顶上
	// （与 httpapi.API.triggerTask 同一兼容处理）。
	IssueKey string
	IssueID  string
	// Title 仅用于回显/展示；当前 tasks 表没有对应列，不落库
	// （见 CreateFlow 文档注释）。
	Title string
	// Priority 就绪集排序用的初始优先级；零值即无提升。
	Priority int
	// DependsOnIndex 是该节点依赖的前驱在本批次里的下标（0-based），
	// nil 表示独立根。契约见 graph.Node。
	DependsOnIndex *int
	// DependsOnAt 是前驱放行的判定时机：'pr_open' 或 'merged'；
	// 空串按 task.CreateParams 的默认值处理（'pr_open'）。
	DependsOnAt string
	// Profile 是节点执行画像的原始 JSON（F7.1，见
	// docs/07-prd-orchestration.md）。类型定为 json.RawMessage 而非
	// runner.Profile 结构体，是刻意的包依赖方向选择：flow 包目前只被
	// runner/httpapi 依赖，反过来让 flow 依赖 runner 会导致
	// httpapi -> flow -> runner -> httpapi 之类的环（runner 已经依赖
	// task 包，flow 也依赖 task 包，二者不该互相依赖）。CreateFlow 把
	// 这段字节原样转给 task.CreateParams.Profile，不解析其内部结构
	// ——解析与校验是 runner.ParseProfile（执行阶段）的职责，建图与
	// 执行两个阶段的职责分工由此保持清楚：建图不因为画像里有个笔误的
	// verify_tier 就拒绝创建，只有真正执行到该节点时才会因校验失败
	// 而任务本身失败。nil 表示未设画像，交给 task.CreateParams.Profile
	// 的 nil 处理逻辑落 schema 默认值 '{}'。
	Profile json.RawMessage
}

// Service 建图并把图落成一批任务。
type Service struct {
	Pool  *pgxpool.Pool
	Tasks *task.Machine
	// Store 用于读取 flow_max_chain_length 系统设置（F3.3-AC2，复用
	// system_settings，migration 0013）。为 nil（未注入）时按"未配置"
	// 处理，直接用默认值 4，不查库——调用方不接这根线也不会报错，只是
	// 拿不到可配置这一半（见 maxChainLength）。
	Store *store.Store
}

// validateNodes 校验批次形状（graph.Validate）与每个节点的业务字段。
func validateNodes(nodes []NodeInput) error {
	graphNodes := make([]Node, len(nodes))
	for i, n := range nodes {
		graphNodes[i] = Node{DependsOnIndex: n.DependsOnIndex}
	}
	if err := Validate(graphNodes); err != nil {
		return err
	}
	for i, n := range nodes {
		if n.IssueKey == "" && n.IssueID == "" {
			return fmt.Errorf("flow: 第 %d 个节点缺少 issueKey/issueId", i)
		}
	}
	return nil
}

// maxChainLength 现取链长上限（F3.3-AC2）。s.Store 为 nil 时直接短路
// 到默认值，不查库——语义上等同于"这个键从未配置过"那一支，只是提前
// 判定，不需要真的往数据库发一次查询。
func (s *Service) maxChainLength(ctx context.Context) int {
	if s.Store == nil {
		return store.DefaultFlowMaxChainLength
	}
	n, err := s.Store.FlowMaxChainLength(ctx)
	if err != nil {
		return store.DefaultFlowMaxChainLength
	}
	return n
}

// chainWarnings 现取链长上限，再委托 chainLengthWarnings 算出本批次里
// 超限节点的警告列表（F3.3）。
func (s *Service) chainWarnings(ctx context.Context, nodes []NodeInput) []string {
	return chainLengthWarnings(nodes, s.maxChainLength(ctx))
}

// chainLengthWarnings 是链长约束的纯逻辑部分：给定上限 maxLen，算出
// 每个深度超限节点对应的一条警告，不涉及任何 DB 访问——供单测在不连库
// 的情况下覆盖 F3.3 的核心判定（CreateFlow 的 warnings 字段就是它的
// 输出，见下方 chainWarnings）。
//
// 不拒绝创建（F3.3-AC1 的"仅警告"精神在无 UI 场景下的落地）：这里只
// 产出字符串，调用方（CreateFlow）照常往下建图。
//
// 警告文案里的节点标识优先用 IssueKey，两者都空则打印空串（不应该
// 发生——validateNodes 已经要求二者至少一个非空）。
func chainLengthWarnings(nodes []NodeInput, maxLen int) []string {
	graphNodes := make([]Node, len(nodes))
	for i, n := range nodes {
		graphNodes[i] = Node{DependsOnIndex: n.DependsOnIndex}
	}
	depths := ChainDepths(graphNodes)

	warnings := make([]string, 0)
	for i, d := range depths {
		if d <= maxLen {
			continue
		}
		key := nodes[i].IssueKey
		if key == "" {
			key = nodes[i].IssueID
		}
		warnings = append(warnings, fmt.Sprintf(
			"节点 %s 所在链长度 %d 超过建议上限 %d", key, d, maxLen))
	}
	return warnings
}

// CreateFlow 建一个编排图并按拓扑序把 nodes 落成一批任务。
//
// 事务/补偿策略：task.Machine.Create 自己开事务并提交，本包不改
// machine.go（避免与另一个同时改 pipeline.go 的 agent 撞文件），因此
// 无法把"建 flow 行 + 建全部任务"纳入同一个数据库事务做"全成功或
// 全不成功"。这里选择补偿式做法：
//  1. 先建 flows 行（一次独立的 INSERT，本身是原子的）；
//  2. 按 nodes 顺序逐个调用 Tasks.Create，每次调用各自提交；
//  3. 如果某个节点创建失败，把本次批次里已经创建成功的任务通过
//     Tasks.Transition 转为 cancelled（queued -> cancelled 是状态机
//     已有的合法边），再返回错误——不删除 flows 行，让它作为"这次
//     批量提交失败，各节点末态是什么"的可审计现场保留下来。
//
// 权衡：与真事务相比，失败窗口内（某节点已提交、尚未来得及补偿）
// 短暂可见一个"部分建好"的 flow；但由于 Machine.Create 对同一 issue
// 有唯一索引兜底，且补偿在同一次请求处理内紧跟着发生，这个窗口极窄，
// 换来的是不touch machine.go 的文件边界约束下最简单可靠的实现。
//
// 幂等（F1.4-AC2）：如果批次第一个节点对应的 issue 已经有一个活任务，
// 且该活任务所属的 flow 与本次请求逐节点对应（issueKey 序列、依赖结构
// 都一致），认为这是"同一批次的重复提交"（人手抖点了两次），直接返回
// 那个已有的 flow 当成功处理，不新建、不报错。只检查第一个节点是为了
// 覆盖"整批重复提交"这个最常见的场景，不追求对任意重叠子集都严谨去重
// ——那需要对批次做整体的图同构比较，收益不匹配这里的复杂度。
//
// 并发去重（F1.4-AC2 的另一半）：detectDuplicateSubmission 的"查
// tasks 表判断是否已存在"与后面"建 flow + 建 tasks"之间天然存在
// 竞态——8 个 goroutine 同时提交同一批次时，都会在彼此的任务还没落库
// 前查到"没有重复"，各自都往下建，产生多个孤儿 flow。CreateFlow 用
// acquireSubmissionLock 拿到的 Postgres 咨询锁把"同一 repo + 批次
// 首个 issue key"的并发提交整体串行化：第二个到达的请求会一直阻塞，
// 直到第一个请求把 flow 行和全部任务行都创建完、释放锁之后才能往下
// 走查重判断，这时它查到的就是第一个请求已提交的结果，走幂等分支
// 返回既有结果，而不是各自都误判"没有重复"。
//
// warnings（F3.3-AC2）：链长超过配置上限（system_settings 的
// flow_max_chain_length，未配置则默认 4）的节点不阻止创建，只在返回值
// 里报一条形如"节点 X 所在链长度 N 超过建议上限 M"的警告。F3.3-AC1
// 要求的"UI 给出明确警告"是 UI 职责，这次没有 UI，能落到的最大程度
// 就是这里——HTTP 层原样透传进 JSON 响应，供未来 M5 的画布 UI 消费。
func (s *Service) CreateFlow(ctx context.Context, ownerUserID, repoID int64, name string, nodes []NodeInput) (int64, []*task.Task, []string, error) {
	if err := validateNodes(nodes); err != nil {
		return 0, nil, nil, err
	}

	warnings := s.chainWarnings(ctx, nodes)

	release, err := s.acquireSubmissionLock(ctx, repoID, nodes[0])
	if err != nil {
		return 0, nil, nil, err
	}
	defer release()

	if dupFlowID, dupTasks, ok, err := s.detectDuplicateSubmission(ctx, ownerUserID, repoID, nodes); err != nil {
		return 0, nil, nil, err
	} else if ok {
		return dupFlowID, dupTasks, warnings, nil
	}

	var flowID int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO flows (user_id, repo_id, name) VALUES ($1, $2, $3) RETURNING id`,
		ownerUserID, repoID, name,
	).Scan(&flowID); err != nil {
		return 0, nil, nil, fmt.Errorf("flow: 创建编排图失败: %w", err)
	}

	created := make([]*task.Task, 0, len(nodes))
	ids := make([]int64, len(nodes))

	for i, n := range nodes {
		var dependsOn *int64
		if n.DependsOnIndex != nil {
			dependsOn = &ids[*n.DependsOnIndex]
		}

		issueKey, issueID := n.IssueKey, n.IssueID
		if issueKey == "" {
			issueKey = issueID
		}
		if issueID == "" {
			issueID = issueKey
		}

		t, err := s.Tasks.Create(ctx, task.CreateParams{
			UserID:         ownerUserID,
			RepoID:         repoID,
			LinearIssueKey: issueKey,
			LinearIssueID:  issueID,
			FlowID:         &flowID,
			DependsOn:      dependsOn,
			DependsOnAt:    n.DependsOnAt,
			Priority:       n.Priority,
			Profile:        []byte(n.Profile),
		})
		if err != nil {
			s.compensate(ctx, created)
			if isUniqueViolation(err) {
				return 0, nil, nil, ErrIssueActive{IssueKey: issueKey}
			}
			return 0, nil, nil, fmt.Errorf("flow: 创建第 %d 个节点（issue %s）失败: %w", i, issueKey, err)
		}
		ids[i] = t.ID
		created = append(created, t)
	}

	return flowID, created, warnings, nil
}

// acquireSubmissionLock 用 Postgres 会话级事务咨询锁
// （pg_advisory_xact_lock）把"同一 repo + 批次首个 issue key"的并发
// CreateFlow 提交串行化，锁 key 是 "repoID:firstIssueKey" 这个字符串
// 的哈希（hashtext）。
//
// 实现上特意用一条"专用"连接 + 专用事务只持有这把锁，不在这个事务里
// 做任何实际写入——真正的 INSERT INTO flows 与 Tasks.Create 仍然各自
// 走自己的连接/事务（未改动，且本包不能碰 machine.go）。这样安排是为
// 了避免死锁：如果锁和"建 flow 行"共用同一个未提交的事务，后续
// Tasks.Create 在别的连接上做外键校验时会等这个事务提交，而这个事务
// 又要等 CreateFlow 里所有 Tasks.Create 都跑完才提交——循环等待。专用
// 锁连接不写任何东西，谁也不需要等它提交才能看见数据，只有第二个并发
// 请求需要等它释放锁才能继续，没有这层耦合就没有死锁风险。
//
// 用 xact 版本（而非 pg_advisory_lock 会话版本）是为了不需要手工调用
// pg_advisory_unlock：调用方拿到的 release 函数只负责让这个专用事务
// "结束"（Commit——事务里没有写入，Commit 与 Rollback 效果一致），
// 释放动作由 Postgres 在事务结束时自动完成，CreateFlow 无论从哪条
// return 路径退出，defer release() 都会执行，不会漏放。release 特意
// 用 context.Background() 而不是调用方传入的 ctx 去做这次收尾提交，
// 避免调用方 ctx 提前取消导致锁释放不掉、把后续请求永久卡住。
func (s *Service) acquireSubmissionLock(ctx context.Context, repoID int64, first NodeInput) (func(), error) {
	firstKey := first.IssueKey
	if firstKey == "" {
		firstKey = first.IssueID
	}
	lockKey := fmt.Sprintf("%d:%s", repoID, firstKey)

	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("flow: 获取提交锁连接失败: %w", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("flow: 开启提交锁事务失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		_ = tx.Rollback(ctx)
		conn.Release()
		return nil, fmt.Errorf("flow: 获取提交锁失败: %w", err)
	}

	return func() {
		_ = tx.Commit(context.Background())
		conn.Release()
	}, nil
}

// compensate 把本次批次里已经创建成功的任务转为 cancelled，
// 供 CreateFlow 在中途失败时补偿"全成功或全不成功"的语义。
func (s *Service) compensate(ctx context.Context, created []*task.Task) {
	for _, t := range created {
		_, _ = s.Tasks.Transition(ctx, t.ID, task.StateCancelled, "system", &task.TransitionOpts{
			Payload: map[string]any{"reason": "flow_create_rollback"},
		})
	}
}

// detectDuplicateSubmission 判定本次请求是否是"同一批次的重复提交"。
//
// 只看第一个节点：如果它对应的 issue 已经有一个活任务，且该活任务
// 挂在某个 flow 下，把那个 flow 的全部任务取出来跟本次 nodes 逐一
// 比较（issueKey 序列 + 依赖结构）。完全一致就认为是重复提交，返回
// 那个已有的结果；否则是真冲突（同一 issue 被两个不同批次占用），
// 交给上面的创建流程在真正尝试建它时报错。
func (s *Service) detectDuplicateSubmission(ctx context.Context, ownerUserID, repoID int64, nodes []NodeInput) (int64, []*task.Task, bool, error) {
	first := nodes[0]
	firstKey := first.IssueKey
	if firstKey == "" {
		firstKey = first.IssueID
	}

	var existingID int64
	var existingFlowID *int64
	err := s.Pool.QueryRow(ctx, `
		SELECT id, flow_id FROM tasks
		WHERE repo_id = $1 AND user_id = $2 AND linear_issue_key = $3
		  AND state NOT IN ('merged', 'failed', 'cancelled')`,
		repoID, ownerUserID, firstKey,
	).Scan(&existingID, &existingFlowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil // 没有活任务占用，正常走创建流程
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("flow: 查询 issue 现有任务失败: %w", err)
	}
	if existingFlowID == nil {
		// 活任务存在但不属于任何 flow：这是一次真冲突，不是重复提交
		return 0, nil, false, ErrIssueActive{IssueKey: firstKey}
	}

	existingTasks, err := s.flowTasksOrdered(ctx, *existingFlowID)
	if err != nil {
		return 0, nil, false, err
	}
	if !sameBatch(existingTasks, nodes) {
		return 0, nil, false, ErrIssueActive{IssueKey: firstKey}
	}
	return *existingFlowID, existingTasks, true, nil
}

// sameBatch 报告 existing（某个已有 flow 下按 id 升序排列的任务）
// 是否与本次请求的 nodes 描述的是同一批次：issueKey 序列相同，
// 且每个节点的依赖结构（依赖前一批次里的哪一个）也相同。
func sameBatch(existing []*task.Task, nodes []NodeInput) bool {
	if len(existing) != len(nodes) {
		return false
	}
	idOf := make(map[int64]int, len(existing))
	for i, t := range existing {
		idOf[t.ID] = i
	}
	for i, n := range nodes {
		key := n.IssueKey
		if key == "" {
			key = n.IssueID
		}
		if existing[i].LinearIssueKey != key {
			return false
		}
		wantDep := n.DependsOnIndex
		if wantDep == nil {
			if existing[i].DependsOn != nil {
				return false
			}
			continue
		}
		if existing[i].DependsOn == nil {
			return false
		}
		gotDepPos, ok := idOf[*existing[i].DependsOn]
		if !ok || gotDepPos != *wantDep {
			return false
		}
	}
	return true
}

// flowTasksOrdered 按 id 升序（即创建顺序）取出某个 flow 下的全部任务。
func (s *Service) flowTasksOrdered(ctx context.Context, flowID int64) ([]*task.Task, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM tasks WHERE flow_id = $1 ORDER BY id`, flowID)
	if err != nil {
		return nil, fmt.Errorf("flow: 查询 flow %d 的任务列表失败: %w", flowID, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("flow: 读取任务 ID 失败: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("flow: 查询 flow %d 的任务列表失败: %w", flowID, err)
	}

	out := make([]*task.Task, 0, len(ids))
	for _, id := range ids {
		t, err := s.Tasks.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("flow: 读取任务 %d 失败: %w", id, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// FlowSummary 是查询一个 flow 时返回的信息：flow 元信息 + 其下全部
// 任务的精简视图。
type FlowSummary struct {
	ID     int64
	RepoID int64
	Name   string
	Tasks  []TaskSummary
}

// TaskSummary 是 flow 下一个任务的精简视图，供查询端点展示状态。
type TaskSummary struct {
	ID        int64
	IssueKey  string
	State     string
	DependsOn *int64
	Priority  int
}

// GetFlow 读取一个 flow 的信息与其下全部任务的当前状态。
//
// 不是自己的 flow 与不存在同等处理（与 API 里其它资源同一原则：
// 对非属主隐瞒存在，排障走数据库）。
func (s *Service) GetFlow(ctx context.Context, ownerUserID, flowID int64) (*FlowSummary, error) {
	var fs FlowSummary
	err := s.Pool.QueryRow(ctx,
		`SELECT id, repo_id, name FROM flows WHERE id = $1 AND user_id = $2`,
		flowID, ownerUserID,
	).Scan(&fs.ID, &fs.RepoID, &fs.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFlowNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("flow: 读取编排图 %d 失败: %w", flowID, err)
	}

	rows, err := s.Pool.Query(ctx,
		`SELECT id, linear_issue_key, state, depends_on, priority
		 FROM tasks WHERE flow_id = $1 ORDER BY id`, flowID)
	if err != nil {
		return nil, fmt.Errorf("flow: 查询编排图 %d 的任务失败: %w", flowID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var ts TaskSummary
		if err := rows.Scan(&ts.ID, &ts.IssueKey, &ts.State, &ts.DependsOn, &ts.Priority); err != nil {
			return nil, fmt.Errorf("flow: 读取任务行失败: %w", err)
		}
		fs.Tasks = append(fs.Tasks, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("flow: 查询编排图 %d 的任务失败: %w", flowID, err)
	}
	return &fs, nil
}

// isUniqueViolation 报告 err 是否是唯一约束冲突（SQLSTATE 23505）。
//
// 在 CreateFlow 的调用路径上，这只可能是 tasks_one_active_per_issue
// ——同一 issue 已有一个活任务，与 store.CreateRepo 翻译唯一约束冲突
// 同一手法。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
