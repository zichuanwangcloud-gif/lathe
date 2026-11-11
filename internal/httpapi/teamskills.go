package httpapi

// teamskills.go 团队自定义技能（f11，docs/features/team-skills.md）的管理接口。
//
// 独立的 API 结构体，与 CredentialAPI 同形（自带 Routes），不往 httpapi.API 上加
// 字段、不改 api.go —— 那是多条并行线都会动的结构体。
//
// 每个 handler 的第一件事是用当前用户定位父行（仓库 / 技能 / 任务），定位不到
// 就是 404：不属于当前用户与不存在同等处理，不出现 403（AGENTS.md §7）。
//
// 错误码：父资源不存在或不是你的 → 404；同仓库同名在役技能 → 409；校验失败、
// 数量或大小超限 → 400（可读中文原因，与 updateRepo 同一风格）。任何 4xx 都不把
// SQLSTATE 或约束名透给用户。
//
// 凭据扫描在三个写入路径（新建 / 新版本 / 恢复）上**每个都调**：登记值 =
// 装配侧 KnownSecrets + 当前用户已保存的集成凭据（只解密、比对、丢弃，不写日志）。
// 拒绝信息只带文件与行号，不带内容。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/teamskills"
)

// TeamSkillAPI 管理团队自定义技能。
type TeamSkillAPI struct {
	Store *store.Store
	// Secrets 用来取当前用户已保存的集成凭据，作为凭据扫描的已知值。
	// 为 nil 时只用 KnownSecrets 与形态表（测试与老装配）。
	Secrets *store.Secrets
	// KnownSecrets 是装配侧登记的平台自身凭据（config.KnownSecrets）。
	KnownSecrets []string
	Auth         *Auth
}

// Routes 注册团队技能接口。
func (a *TeamSkillAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/repos/{id}/team-skills", a.Auth.RequireFunc(a.list))
	mux.Handle("POST /api/repos/{id}/team-skills", a.Auth.RequireFunc(a.create))
	mux.Handle("GET /api/repos/{id}/team-skills/{sid}", a.Auth.RequireFunc(a.get))
	mux.Handle("PATCH /api/repos/{id}/team-skills/{sid}", a.Auth.RequireFunc(a.patch))
	mux.Handle("DELETE /api/repos/{id}/team-skills/{sid}", a.Auth.RequireFunc(a.archive))
	mux.Handle("GET /api/repos/{id}/team-skills/{sid}/versions/{v}", a.Auth.RequireFunc(a.version))
	mux.Handle("POST /api/repos/{id}/team-skills/{sid}/versions", a.Auth.RequireFunc(a.appendVersion))
	mux.Handle("POST /api/repos/{id}/team-skills/{sid}/versions/{v}/restore", a.Auth.RequireFunc(a.restore))
	mux.Handle("GET /api/tasks/{id}/team-skills", a.Auth.RequireFunc(a.taskSkills))
}

func notFound(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": what + "不存在"})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
}

// pathInt 解析路径参数；非法值按「不存在」处理（404），不暴露格式细节之外的信息。
func pathInt(r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return v, err == nil && v > 0
}

// repoOf 校验路径里的仓库属于当前用户；否则写 404 并返回 false。
func (a *TeamSkillAPI) repoOf(w http.ResponseWriter, r *http.Request) (userID, repoID int64, ok bool) {
	userID = CurrentUser(r).ID
	repoID, ok = pathInt(r, "id")
	if !ok {
		notFound(w, "仓库")
		return 0, 0, false
	}
	if _, err := a.Store.GetRepo(r.Context(), repoID, userID); err != nil {
		if errors.Is(err, store.ErrRepoNotFound) {
			notFound(w, "仓库")
		} else {
			serverError(w, "读取仓库失败", err)
		}
		return 0, 0, false
	}
	return userID, repoID, true
}

// writeStoreErr 把 store 的哨兵错误映射成 HTTP 状态码。
func writeStoreErr(w http.ResponseWriter, msg string, err error) {
	switch {
	case errors.Is(err, store.ErrRepoNotFound):
		notFound(w, "仓库")
	case errors.Is(err, store.ErrTeamSkillNotFound):
		notFound(w, "技能")
	case errors.Is(err, store.ErrTeamSkillVersionNotFound):
		notFound(w, "技能版本")
	case errors.Is(err, store.ErrTeamSkillSlugTaken):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "这个仓库已有同名的技能，请换一个名字（或先归档旧的）"})
	case errors.Is(err, store.ErrTeamSkillLimit):
		badRequest(w, fmt.Sprintf("每个仓库最多 %d 个在役技能，请先归档不再使用的技能", teamskills.MaxSkillsPerRepo))
	default:
		serverError(w, msg, err)
	}
}

// prepareVersion 校验文件、note 并做凭据扫描，返回可落库的版本内容。
// 失败时已写好 400 响应并返回 false。
func (a *TeamSkillAPI) prepareVersion(ctx context.Context, w http.ResponseWriter, userID int64, files []teamskills.File, note string) (store.NewTeamSkillVersion, bool) {
	sorted, total, err := teamskills.ValidateFiles(files)
	if err != nil {
		badRequest(w, err.Error())
		return store.NewTeamSkillVersion{}, false
	}
	if _, err := teamskills.ValidateNote(note); err != nil {
		badRequest(w, err.Error())
		return store.NewTeamSkillVersion{}, false
	}
	registered, err := a.registeredSecrets(ctx, userID)
	if err != nil {
		serverError(w, "准备凭据扫描失败", err)
		return store.NewTeamSkillVersion{}, false
	}
	for _, f := range sorted {
		if line, hit := runner.FindCredential(f.Content, registered); hit {
			// 只记文件、行号，不记值（AGENTS.md §7）。
			slog.Warn("团队技能内容疑似含凭据，已拒绝保存", "user", userID, "path", f.Path, "line", line)
			badRequest(w, runner.CredentialRejectMessage(f.Path, line))
			return store.NewTeamSkillVersion{}, false
		}
	}
	return store.NewTeamSkillVersion{
		Files: sorted, SHA256: teamskills.ContentSHA256(sorted), TotalBytes: total, Note: note,
	}, true
}

// registeredSecrets 汇总凭据扫描层 1 的已知值：部署自身的凭据 + 当前用户已保存
// 的集成凭据。只在内存里比对，用完即弃。
func (a *TeamSkillAPI) registeredSecrets(ctx context.Context, userID int64) ([]string, error) {
	out := append([]string(nil), a.KnownSecrets...)
	if a.Secrets == nil {
		return out, nil
	}
	for _, kind := range store.KnownKinds {
		v, err := a.Secrets.Get(ctx, userID, kind)
		if errors.Is(err, store.ErrIntegrationNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (a *TeamSkillAPI) list(w http.ResponseWriter, r *http.Request) {
	userID, repoID, ok := a.repoOf(w, r)
	if !ok {
		return
	}
	skills, err := a.Store.ListTeamSkills(r.Context(), userID, repoID)
	if err != nil {
		serverError(w, "查询团队技能失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

type createBody struct {
	Slug  string            `json:"slug"`
	Title string            `json:"title"`
	Files []teamskills.File `json:"files"`
	Note  string            `json:"note"`
}

func (a *TeamSkillAPI) create(w http.ResponseWriter, r *http.Request) {
	userID, repoID, ok := a.repoOf(w, r)
	if !ok {
		return
	}
	var body createBody
	if err := decodeJSON(r, &body); err != nil {
		badRequest(w, "请求体格式错误（或超过 1 MiB）")
		return
	}
	slug, err := teamskills.NormalizeSlug(body.Slug)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if _, err := teamskills.ValidateTitle(body.Title); err != nil {
		badRequest(w, err.Error())
		return
	}
	v, ok := a.prepareVersion(r.Context(), w, userID, body.Files, body.Note)
	if !ok {
		return
	}
	sk, err := a.Store.CreateTeamSkill(r.Context(), userID, repoID, slug, body.Title, v)
	if err != nil {
		writeStoreErr(w, "新建团队技能失败", err)
		return
	}
	slog.Info("新建团队技能", "user", userID, "repo", repoID, "slug", slug, "version", 1, "sha256", v.SHA256, "bytes", v.TotalBytes)
	writeJSON(w, http.StatusCreated, map[string]any{"skill": sk, "version": 1})
}

func (a *TeamSkillAPI) skillOf(w http.ResponseWriter, r *http.Request) (userID, repoID, skillID int64, ok bool) {
	userID, repoID, ok = a.repoOf(w, r)
	if !ok {
		return 0, 0, 0, false
	}
	skillID, ok = pathInt(r, "sid")
	if !ok {
		notFound(w, "技能")
		return 0, 0, 0, false
	}
	return userID, repoID, skillID, true
}

func (a *TeamSkillAPI) get(w http.ResponseWriter, r *http.Request) {
	userID, repoID, skillID, ok := a.skillOf(w, r)
	if !ok {
		return
	}
	sk, versions, err := a.Store.GetTeamSkill(r.Context(), userID, repoID, skillID)
	if err != nil {
		writeStoreErr(w, "读取团队技能失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": sk, "versions": versions})
}

func (a *TeamSkillAPI) version(w http.ResponseWriter, r *http.Request) {
	userID, repoID, skillID, ok := a.skillOf(w, r)
	if !ok {
		return
	}
	v, ok := pathInt(r, "v")
	if !ok {
		notFound(w, "技能版本")
		return
	}
	ver, err := a.Store.GetTeamSkillVersion(r.Context(), userID, repoID, skillID, int(v))
	if err != nil {
		writeStoreErr(w, "读取技能版本失败", err)
		return
	}
	writeJSON(w, http.StatusOK, ver)
}

// writeAppended 统一「产生新版本 → 201 / 内容未变 → 200」的响应。
func writeAppended(w http.ResponseWriter, version int, created bool) {
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"version": version, "created": created})
}

func (a *TeamSkillAPI) appendVersion(w http.ResponseWriter, r *http.Request) {
	userID, repoID, skillID, ok := a.skillOf(w, r)
	if !ok {
		return
	}
	var body struct {
		Files []teamskills.File `json:"files"`
		Note  string            `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		badRequest(w, "请求体格式错误（或超过 1 MiB）")
		return
	}
	// 先确认技能存在且属于当前用户，再做校验与凭据扫描：别人的技能 ID 一律 404，
	// 不让校验错误泄露「这个 ID 存在」。
	if _, _, err := a.Store.GetTeamSkill(r.Context(), userID, repoID, skillID); err != nil {
		writeStoreErr(w, "读取团队技能失败", err)
		return
	}
	v, ok := a.prepareVersion(r.Context(), w, userID, body.Files, body.Note)
	if !ok {
		return
	}
	version, created, err := a.Store.AppendTeamSkillVersion(r.Context(), userID, repoID, skillID, v)
	if err != nil {
		writeStoreErr(w, "写入技能版本失败", err)
		return
	}
	if created {
		slog.Info("团队技能新版本", "user", userID, "skill", skillID, "version", version, "sha256", v.SHA256, "bytes", v.TotalBytes)
	}
	writeAppended(w, version, created)
}

// restore 把旧版内容作为新版本提交（版本号继续增），不修改历史行。
// 恢复的内容同样过一遍校验与凭据扫描：保存后用户可能新登记了集成凭据。
func (a *TeamSkillAPI) restore(w http.ResponseWriter, r *http.Request) {
	userID, repoID, skillID, ok := a.skillOf(w, r)
	if !ok {
		return
	}
	v, ok := pathInt(r, "v")
	if !ok {
		notFound(w, "技能版本")
		return
	}
	old, err := a.Store.GetTeamSkillVersion(r.Context(), userID, repoID, skillID, int(v))
	if err != nil {
		writeStoreErr(w, "读取技能版本失败", err)
		return
	}
	nv, ok := a.prepareVersion(r.Context(), w, userID, old.Files, fmt.Sprintf("恢复自 v%d", v))
	if !ok {
		return
	}
	version, created, err := a.Store.AppendTeamSkillVersion(r.Context(), userID, repoID, skillID, nv)
	if err != nil {
		writeStoreErr(w, "恢复技能版本失败", err)
		return
	}
	writeAppended(w, version, created)
}

func (a *TeamSkillAPI) patch(w http.ResponseWriter, r *http.Request) {
	userID, repoID, skillID, ok := a.skillOf(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool   `json:"enabled"`
		Title   *string `json:"title"`
	}
	if err := decodeJSON(r, &body); err != nil {
		badRequest(w, "请求体格式错误")
		return
	}
	if body.Title != nil {
		if _, err := teamskills.ValidateTitle(*body.Title); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	sk, err := a.Store.UpdateTeamSkill(r.Context(), userID, repoID, skillID, body.Enabled, body.Title)
	if err != nil {
		writeStoreErr(w, "更新团队技能失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": sk})
}

func (a *TeamSkillAPI) archive(w http.ResponseWriter, r *http.Request) {
	userID, repoID, skillID, ok := a.skillOf(w, r)
	if !ok {
		return
	}
	if err := a.Store.ArchiveTeamSkill(r.Context(), userID, repoID, skillID); err != nil {
		writeStoreErr(w, "归档团队技能失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"archived": true})
}

// taskSkills 返回任务钉住的技能与版本。pinned=false 表示还没钉（任务尚未进入
// 实现阶段，或功能关闭）。非属主 404。
func (a *TeamSkillAPI) taskSkills(w http.ResponseWriter, r *http.Request) {
	taskID, ok := pathInt(r, "id")
	if !ok {
		notFound(w, "任务")
		return
	}
	pins, pinned, err := a.Store.TaskTeamSkills(r.Context(), taskID, CurrentUser(r).ID)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			notFound(w, "任务")
			return
		}
		serverError(w, "读取任务钉住的技能失败", err)
		return
	}
	if pins == nil {
		pins = []store.TeamSkillPin{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pinned": pinned, "skills": pins})
}
