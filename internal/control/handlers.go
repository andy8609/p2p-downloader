package control

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"p2pdl/internal/app"
	"p2pdl/internal/engine"
	"p2pdl/internal/version"
)

// ---------------------------------------------------------------------------
// 基础端点
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": version.Version,
		"time":    nowRFC3339(),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  version.Version,
		"commit":   version.Commit,
		"builtAt":  version.BuildTime,
		"go":       version.GoVersion(),
		"platform": version.Platform(),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.app.Scheduler().Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":     stats,
		"dataDir":   s.app.Options().DataDir,
		"rateLimit": s.app.GlobalRateLimit(),
	})
}

// ---------------------------------------------------------------------------
// 任务端点
// ---------------------------------------------------------------------------

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	views := s.app.Scheduler().List()
	if views == nil {
		views = []engine.TaskView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": views,
		"total": len(views),
	})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	view, ok := s.app.Scheduler().Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "task_not_found", "任务不存在: "+id)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// createTaskRequest 是新建任务的请求体。
type createTaskRequest struct {
	// Kind 是 "http" | "bittorrent"（可按 source 自动判断，缺省时必填其一）。
	Kind          string `json:"kind"`
	// Source 是 URL 或磁力链。
	Source        string `json:"source"`
	// TorrentPath 是 .torrent 文件路径（与 Source 二选一）。
	TorrentPath   string `json:"torrentPath"`
	Name          string `json:"name"`
	DestPath      string `json:"destPath"`
	Header        map[string]string `json:"header"`
	Peers         []string `json:"peers"`
	ExpectedHash  string `json:"expectedHash"`
	HashAlgorithm string `json:"hashAlgorithm"`
	Paused        bool   `json:"paused"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	source := strings.TrimSpace(req.Source)
	torrentPath := strings.TrimSpace(req.TorrentPath)
	if source == "" && torrentPath == "" {
		writeError(w, http.StatusBadRequest, "missing_source", "必须提供 source（URL/磁力链）或 torrentPath")
		return
	}

	opts := app.AddOptions{
		Name:          req.Name,
		DestPath:      req.DestPath,
		Header:        req.Header,
		Peers:         req.Peers,
		ExpectedHash:  req.ExpectedHash,
		HashAlgorithm: req.HashAlgorithm,
		Paused:        req.Paused,
	}

	var (
		task *engine.Task
		err  error
	)
	switch {
	case torrentPath != "":
		task, err = s.app.AddTorrentFile(r.Context(), torrentPath, opts)
	case strings.HasPrefix(source, "magnet:"):
		task, err = s.app.AddMagnet(r.Context(), source, opts)
	default:
		task, err = s.app.AddHTTP(r.Context(), source, opts)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "add_failed", err.Error())
		return
	}

	view, _ := s.app.Scheduler().Get(task.ID)
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handlePauseTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.app.Scheduler().Pause(id); err != nil {
		writeError(w, http.StatusConflict, "pause_failed", err.Error())
		return
	}
	s.respondTask(w, id)
}

func (s *Server) handleResumeTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.app.Scheduler().Resume(id); err != nil {
		writeError(w, http.StatusConflict, "resume_failed", err.Error())
		return
	}
	s.respondTask(w, id)
}

func (s *Server) handleRecheckTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.app.Recheck(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, "recheck_failed", err.Error())
		return
	}
	s.respondTask(w, id)
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deleteFiles := false
	if raw := r.URL.Query().Get("files"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", "files 参数应为布尔值")
			return
		}
		deleteFiles = parsed
	}

	if err := s.app.Remove(r.Context(), id, deleteFiles); err != nil {
		writeError(w, http.StatusConflict, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "removed": true, "filesDeleted": deleteFiles})
}

func (s *Server) respondTask(w http.ResponseWriter, id string) {
	view, ok := s.app.Scheduler().Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "task_not_found", "任务不存在: "+id)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ---------------------------------------------------------------------------
// 设置端点
// ---------------------------------------------------------------------------

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	opts := s.app.Options()
	writeJSON(w, http.StatusOK, map[string]any{
		"dataDir":            opts.DataDir,
		"metaDir":            opts.MetaDir,
		"chunkSize":          opts.ChunkSize,
		"concurrency":        opts.Concurrency,
		"maxConcurrentTasks": opts.MaxConcurrentTasks,
		"globalRateLimit":    s.app.GlobalRateLimit(),
		"maxMemory":          opts.MaxMemory,
		"btListenPort":       opts.BT.ListenPort,
		"buildManifest":      opts.BuildManifestOnSuccess,
	})
}

// patchSettingsRequest 只允许修改运行期可安全变更的设置。
//
// 说明：DataDir、ChunkSize 等会影响已存在任务的数据布局，修改它们必须在
// 没有任务运行时重建内核，因此不通过控制面暴露。
type patchSettingsRequest struct {
	GlobalRateLimit *int64 `json:"globalRateLimit"`
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var req patchSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.GlobalRateLimit != nil {
		if *req.GlobalRateLimit < 0 {
			writeError(w, http.StatusBadRequest, "invalid_value", "限速不能为负数")
			return
		}
		s.app.SetGlobalRateLimit(*req.GlobalRateLimit)
	}
	s.handleGetSettings(w, r)
}

func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z07:00")
}
