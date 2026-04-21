package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codernirdesh/pgdrive-backup/internal/config"
	"github.com/codernirdesh/pgdrive-backup/internal/decryptor"
	"github.com/codernirdesh/pgdrive-backup/internal/pipeline"
	"github.com/codernirdesh/pgdrive-backup/internal/uploader"
)

// ── Job tracking ─────────────────────────────────────────────────────────────

type jobStatus string

const (
	jobRunning jobStatus = "running"
	jobSuccess jobStatus = "success"
	jobFailed  jobStatus = "failed"
)

type JobRun struct {
	ID         int
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
	Status     jobStatus
	ErrMsg     string

	mu    sync.Mutex
	logs  []string
	logCh chan string
}

func newJob(id int) *JobRun {
	return &JobRun{
		ID:        id,
		StartedAt: time.Now(),
		Status:    jobRunning,
		logCh:     make(chan string, 512),
	}
}

func (j *JobRun) appendLog(msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.logs = append(j.logs, msg)
	if j.Status == jobRunning {
		select {
		case j.logCh <- msg:
		default:
		}
	}
}

func (j *JobRun) finish(status jobStatus, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = status
	j.ErrMsg = errMsg
	j.FinishedAt = time.Now()
	j.Duration = j.FinishedAt.Sub(j.StartedAt)
	close(j.logCh)
}

func (j *JobRun) snapshot() (logs []string, status jobStatus, done bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := make([]string, len(j.logs))
	copy(cp, j.logs)
	return cp, j.Status, j.Status != jobRunning
}

// ── Schedule ──────────────────────────────────────────────────────────────────

type scheduleConfig struct {
	Enabled      bool      `json:"enabled"`
	IntervalSecs int64     `json:"interval_secs"`
	NextRun      time.Time `json:"next_run"`
	LastRun      time.Time `json:"last_run"`
	LastStatus   string    `json:"last_status"`
}

const scheduleFile = "schedule.json"
const settingsFile = "settings.json"

// ── Server config ─────────────────────────────────────────────────────────────

type appSettings struct {
	Addr          string `json:"addr"`
	Credentials   string `json:"credentials"`
	FolderID      string `json:"folder_id"`
	TokenFile     string `json:"token_file"`
	Prefix        string `json:"prefix"`
	Key           string `json:"key"`
	PGHost        string `json:"pg_host"`
	PGPort        string `json:"pg_port"`
	PGUser        string `json:"pg_user"`
	PGPassword    string `json:"pg_password"`
	PGDatabase    string `json:"pg_database"`
	RetentionDays int    `json:"retention_days"`
}

// loadSettings returns settings by merging defaults → env vars → settings.json.
// The settings.json (written by the UI) takes highest precedence.
func loadSettings() appSettings {
	s := appSettings{
		Addr:          ":8080",
		Credentials:   "credentials.json",
		TokenFile:     "token.json",
		Prefix:        "pgbackup",
		PGPort:        "5432",
		RetentionDays: 7,
	}
	// Overlay env vars
	if v := envOr("WEB_ADDR", ""); v != "" {
		s.Addr = v
	}
	if v := envOr("GOOGLE_DRIVE_CREDENTIALS", ""); v != "" {
		s.Credentials = v
	}
	if v := envOr("GOOGLE_DRIVE_FOLDER_ID", ""); v != "" {
		s.FolderID = v
	}
	if v := envOr("GOOGLE_DRIVE_TOKEN_FILE", ""); v != "" {
		s.TokenFile = v
	}
	if v := envOr("BACKUP_PREFIX", ""); v != "" {
		s.Prefix = v
	}
	if v := envOr("ENCRYPTION_KEY", ""); v != "" {
		s.Key = v
	}
	if v := envOr("PG_HOST", ""); v != "" {
		s.PGHost = v
	}
	if v := envOr("PG_PORT", ""); v != "" {
		s.PGPort = v
	}
	if v := envOr("PG_USER", ""); v != "" {
		s.PGUser = v
	}
	if v := envOr("PG_PASSWORD", ""); v != "" {
		s.PGPassword = v
	}
	if v := envOr("PG_DATABASE", ""); v != "" {
		s.PGDatabase = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("RETENTION_DAYS"))); err == nil && v > 0 {
		s.RetentionDays = v
	}
	// settings.json (UI-saved) overrides env vars
	if data, err := os.ReadFile(settingsFile); err == nil {
		var saved appSettings
		if json.Unmarshal(data, &saved) == nil {
			if saved.Addr != "" {
				s.Addr = saved.Addr
			}
			if saved.Credentials != "" {
				s.Credentials = saved.Credentials
			}
			if saved.FolderID != "" {
				s.FolderID = saved.FolderID
			}
			if saved.TokenFile != "" {
				s.TokenFile = saved.TokenFile
			}
			if saved.Prefix != "" {
				s.Prefix = saved.Prefix
			}
			if saved.Key != "" {
				s.Key = saved.Key
			}
			if saved.PGHost != "" {
				s.PGHost = saved.PGHost
			}
			if saved.PGPort != "" {
				s.PGPort = saved.PGPort
			}
			if saved.PGUser != "" {
				s.PGUser = saved.PGUser
			}
			if saved.PGPassword != "" {
				s.PGPassword = saved.PGPassword
			}
			if saved.PGDatabase != "" {
				s.PGDatabase = saved.PGDatabase
			}
			if saved.RetentionDays > 0 {
				s.RetentionDays = saved.RetentionDays
			}
		}
	}
	return s
}

func (c appSettings) toBackupConfig() (*config.Config, error) {
	missing := []string{}
	if c.PGHost == "" {
		missing = append(missing, "PG_HOST")
	}
	if c.PGUser == "" {
		missing = append(missing, "PG_USER")
	}
	if c.PGPassword == "" {
		missing = append(missing, "PG_PASSWORD")
	}
	if c.PGDatabase == "" {
		missing = append(missing, "PG_DATABASE")
	}
	if c.Key == "" {
		missing = append(missing, "ENCRYPTION_KEY")
	}
	if c.FolderID == "" {
		missing = append(missing, "GOOGLE_DRIVE_FOLDER_ID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	if len(c.Key) != 64 {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be 64 hex chars")
	}
	return &config.Config{
		PGHost:                 c.PGHost,
		PGPort:                 c.PGPort,
		PGUser:                 c.PGUser,
		PGPassword:             c.PGPassword,
		PGDatabase:             c.PGDatabase,
		EncryptionKey:          c.Key,
		GoogleDriveCredentials: c.Credentials,
		GoogleDriveFolderID:    c.FolderID,
		GoogleDriveTokenFile:   c.TokenFile,
		RetentionDays:          c.RetentionDays,
		BackupPrefix:           c.Prefix,
	}, nil
}

// ── App ───────────────────────────────────────────────────────────────────────

type app struct {
	cfgMu    sync.RWMutex
	cfg      appSettings
	oauthCfg *uploader.OAuthConfig
	tmpl     *template.Template

	clientMu sync.RWMutex
	client   *uploader.Client

	jobsMu    sync.RWMutex
	jobs      []*JobRun
	jobSeq    int
	activeJob *JobRun

	scheduleMu   sync.Mutex
	schedule     scheduleConfig
	schedulerCtx context.CancelFunc
}

func (a *app) getClient() *uploader.Client {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.client
}

func (a *app) setClient(c *uploader.Client) {
	a.clientMu.Lock()
	a.client = c
	a.clientMu.Unlock()
}

func (a *app) startJob() (*JobRun, error) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	if a.activeJob != nil && a.activeJob.Status == jobRunning {
		return nil, fmt.Errorf("a backup is already running (job #%d)", a.activeJob.ID)
	}
	a.jobSeq++
	job := newJob(a.jobSeq)
	a.activeJob = job
	a.jobs = append([]*JobRun{job}, a.jobs...)
	if len(a.jobs) > 50 {
		a.jobs = a.jobs[:50]
	}
	return job, nil
}

func (a *app) currentJob() *JobRun {
	a.jobsMu.RLock()
	defer a.jobsMu.RUnlock()
	return a.activeJob
}

func (a *app) allJobs() []*JobRun {
	a.jobsMu.RLock()
	defer a.jobsMu.RUnlock()
	cp := make([]*JobRun, len(a.jobs))
	copy(cp, a.jobs)
	return cp
}

// ── View types ────────────────────────────────────────────────────────────────

type viewData struct {
	Page     string
	HasKey   bool
	HasToken bool
	Backups  []uploader.BackupFile
	Latest   *uploader.BackupFile
	Prefix   string
	Schedule scheduleConfig
	Jobs     []*JobRun
	Settings appSettings
	OAuthURL string
	Flash    string
	Error    string
}

func (a *app) baseData(page string) viewData {
	a.cfgMu.RLock()
	cfg := a.cfg
	a.cfgMu.RUnlock()
	return viewData{
		Page:     page,
		HasKey:   cfg.Key != "",
		HasToken: uploader.HasToken(cfg.TokenFile),
		Prefix:   cfg.Prefix,
		Settings: cfg,
	}
}

func (a *app) saveSettings() error {
	a.cfgMu.RLock()
	data, err := json.MarshalIndent(a.cfg, "", "  ")
	a.cfgMu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(settingsFile, data, 0600)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[web] ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := loadSettings()

	oauthCfg, err := uploader.LoadOAuthConfig(cfg.Credentials)
	if err != nil {
		log.Printf("warn: cannot load OAuth config from %s: %v", cfg.Credentials, err)
	}

	a := &app{cfg: cfg, oauthCfg: oauthCfg}
	a.loadSchedule()

	if uploader.HasToken(cfg.TokenFile) {
		if c, err := uploader.New(ctx, cfg.Credentials, cfg.FolderID, cfg.TokenFile); err == nil {
			a.client = c
			log.Println("Google Drive connected")
		} else {
			log.Printf("warn: drive connect: %v", err)
		}
	} else {
		log.Println("no Google token found — visit /oauth to authorize")
	}

	a.tmpl = buildTemplates()

	if a.schedule.Enabled {
		a.startScheduler(ctx)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/backups", http.StatusFound)
	})
	mux.HandleFunc("/backups", a.handleBackups)
	mux.HandleFunc("/run", a.handleRunPage)
	mux.HandleFunc("/schedule", a.handleSchedulePage)
	mux.HandleFunc("/logs", a.handleLogsPage)
	mux.HandleFunc("/settings", a.handleSettingsPage)
	mux.HandleFunc("/oauth", a.handleOAuthPage)
	mux.HandleFunc("/oauth/callback", a.handleOAuthCallback)
	mux.HandleFunc("/api/backup/run", a.apiRunBackup)
	mux.HandleFunc("/api/backup/stream", a.apiBackupStream)
	mux.HandleFunc("/api/backup/status", a.apiBackupStatus)
	mux.HandleFunc("/api/schedule/save", a.apiSaveSchedule)
	mux.HandleFunc("/api/settings/save", a.apiSaveSettings)
	mux.HandleFunc("/download", a.handleDownloadEncrypted)
	mux.HandleFunc("/decrypt", a.handleDownloadDecrypted)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		a.saveSchedule()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("pgdrive web UI → http://localhost%s", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
	log.Println("server stopped")
}

// ── Page handlers ─────────────────────────────────────────────────────────────

func (a *app) handleBackups(w http.ResponseWriter, r *http.Request) {
	d := a.baseData("backups")
	client := a.getClient()
	if client == nil {
		d.Error = "Google Drive not connected. Visit Setup to authorize."
		a.render(w, d)
		return
	}
	backups, err := client.ListBackups(r.Context(), a.cfg.Prefix)
	if err != nil {
		d.Error = err.Error()
		a.render(w, d)
		return
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedTime.After(backups[j].CreatedTime)
	})
	d.Backups = backups
	if len(backups) > 0 {
		d.Latest = &backups[0]
	}
	a.render(w, d)
}

func (a *app) handleRunPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData("run")
	a.render(w, d)
}

func (a *app) handleSchedulePage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData("schedule")
	a.scheduleMu.Lock()
	d.Schedule = a.schedule
	a.scheduleMu.Unlock()
	if r.URL.Query().Get("saved") == "1" {
		d.Flash = "Schedule saved."
	}
	a.render(w, d)
}

func (a *app) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData("logs")
	d.Jobs = a.allJobs()
	a.render(w, d)
}

func (a *app) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData("settings")
	if r.URL.Query().Get("saved") == "1" {
		d.Flash = "Settings saved."
	}
	a.render(w, d)
}

func (a *app) handleOAuthPage(w http.ResponseWriter, r *http.Request) {
	d := a.baseData("oauth")
	if r.URL.Query().Get("success") == "1" {
		d.Flash = "Google Drive connected successfully!"
	}
	if a.oauthCfg != nil {
		d.OAuthURL = a.oauthCfg.AuthURL("pgdrive-state")
	}
	a.render(w, d)
}

func (a *app) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing OAuth code", http.StatusBadRequest)
		return
	}
	if a.oauthCfg == nil {
		http.Error(w, "OAuth not configured", http.StatusInternalServerError)
		return
	}
	if err := a.oauthCfg.Exchange(r.Context(), code, a.cfg.TokenFile); err != nil {
		http.Error(w, "OAuth exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if c, err := uploader.New(r.Context(), a.cfg.Credentials, a.cfg.FolderID, a.cfg.TokenFile); err == nil {
		a.setClient(c)
		log.Println("Google Drive re-connected after OAuth")
	}
	http.Redirect(w, r, "/oauth?success=1", http.StatusFound)
}

// ── API handlers ──────────────────────────────────────────────────────────────

func (a *app) apiRunBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	backupCfg, err := a.cfg.toBackupConfig()
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	job, err := a.startJob()
	if err != nil {
		jsonErr(w, err.Error(), http.StatusConflict)
		return
	}
	go func() {
		runErr := pipeline.RunWithLogger(context.Background(), backupCfg, job.appendLog)
		if runErr != nil {
			job.finish(jobFailed, runErr.Error())
			a.updateScheduleLastRun("failed")
		} else {
			job.finish(jobSuccess, "")
			a.updateScheduleLastRun("success")
		}
		a.saveSchedule()
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"job_id": job.ID})
}

func (a *app) apiBackupStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	job := a.currentJob()
	if job == nil {
		fmt.Fprintf(w, "data: {\"done\":true,\"status\":\"none\"}\n\n")
		flusher.Flush()
		return
	}

	logs, status, done := job.snapshot()
	snapshotLen := len(logs)
	for _, line := range logs {
		b, _ := json.Marshal(map[string]string{"log": line})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	flusher.Flush()

	if done {
		b, _ := json.Marshal(map[string]any{"done": true, "status": string(status)})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		return
	}

	// Drain the items already sent via the snapshot from the channel so they
	// are not emitted a second time.
	for i := 0; i < snapshotLen; i++ {
		select {
		case _, ok := <-job.logCh:
			if !ok {
				// Job finished while draining; send done and return.
				job.mu.Lock()
				s := job.Status
				errMsg := job.ErrMsg
				job.mu.Unlock()
				payload := map[string]any{"done": true, "status": string(s)}
				if errMsg != "" {
					payload["error"] = errMsg
				}
				b, _ := json.Marshal(payload)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
				return
			}
		case <-r.Context().Done():
			return
		}
	}

	for {
		select {
		case line, ok := <-job.logCh:
			if !ok {
				job.mu.Lock()
				s := job.Status
				errMsg := job.ErrMsg
				job.mu.Unlock()
				payload := map[string]any{"done": true, "status": string(s)}
				if errMsg != "" {
					payload["error"] = errMsg
				}
				b, _ := json.Marshal(payload)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
				return
			}
			b, _ := json.Marshal(map[string]string{"log": line})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *app) apiBackupStatus(w http.ResponseWriter, r *http.Request) {
	job := a.currentJob()
	if job == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "idle"})
		return
	}
	logs, status, done := job.snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"job_id": job.ID,
		"status": string(status),
		"done":   done,
		"logs":   logs,
	})
}

func (a *app) apiSaveSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	intervalStr := r.FormValue("interval")
	enabled := r.FormValue("enabled") == "1"
	d, err := time.ParseDuration(intervalStr)
	if err != nil || d <= 0 {
		jsonErr(w, "invalid interval", http.StatusBadRequest)
		return
	}
	a.scheduleMu.Lock()
	a.schedule.Enabled = enabled
	a.schedule.IntervalSecs = int64(d.Seconds())
	if enabled {
		a.schedule.NextRun = time.Now().Add(d)
	}
	a.scheduleMu.Unlock()
	a.saveSchedule()

	if a.schedulerCtx != nil {
		a.schedulerCtx()
	}
	if enabled {
		schedCtx, cancel := context.WithCancel(context.Background())
		a.schedulerCtx = cancel
		a.startSchedulerWithCtx(schedCtx)
	}
	http.Redirect(w, r, "/schedule?saved=1", http.StatusFound)
}

func (a *app) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	retDays := 7
	if v, err := strconv.Atoi(strings.TrimSpace(r.FormValue("retention_days"))); err == nil && v > 0 {
		retDays = v
	}
	a.cfgMu.Lock()
	a.cfg.PGHost = strings.TrimSpace(r.FormValue("pg_host"))
	a.cfg.PGPort = strings.TrimSpace(r.FormValue("pg_port"))
	if a.cfg.PGPort == "" {
		a.cfg.PGPort = "5432"
	}
	a.cfg.PGUser = strings.TrimSpace(r.FormValue("pg_user"))
	a.cfg.PGPassword = r.FormValue("pg_password")
	a.cfg.PGDatabase = strings.TrimSpace(r.FormValue("pg_database"))
	a.cfg.FolderID = strings.TrimSpace(r.FormValue("folder_id"))
	a.cfg.Prefix = strings.TrimSpace(r.FormValue("prefix"))
	if a.cfg.Prefix == "" {
		a.cfg.Prefix = "pgbackup"
	}
	a.cfg.Credentials = strings.TrimSpace(r.FormValue("credentials"))
	if a.cfg.Credentials == "" {
		a.cfg.Credentials = "credentials.json"
	}
	a.cfg.TokenFile = strings.TrimSpace(r.FormValue("token_file"))
	if a.cfg.TokenFile == "" {
		a.cfg.TokenFile = "token.json"
	}
	a.cfg.RetentionDays = retDays
	// Only overwrite key if a non-empty value was submitted
	if k := strings.TrimSpace(r.FormValue("key")); k != "" {
		a.cfg.Key = k
	}
	cfg := a.cfg
	a.cfgMu.Unlock()

	if err := a.saveSettings(); err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Reload OAuth config with potentially new credentials path
	if oCfg, err := uploader.LoadOAuthConfig(cfg.Credentials); err == nil {
		a.oauthCfg = oCfg
	}
	// Reconnect Drive client with potentially new settings
	if uploader.HasToken(cfg.TokenFile) && cfg.FolderID != "" {
		if c, err := uploader.New(context.Background(), cfg.Credentials, cfg.FolderID, cfg.TokenFile); err == nil {
			a.setClient(c)
			log.Println("Drive client reconnected after settings save")
		}
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusFound)
}

// ── Download handlers ──────────────────────────────────────────────────────────

func (a *app) handleDownloadEncrypted(w http.ResponseWriter, r *http.Request) {
	backup, err := a.lookupBackup(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client := a.getClient()
	if client == nil {
		http.Error(w, "Drive not connected", http.StatusServiceUnavailable)
		return
	}
	reader, err := client.Download(r.Context(), backup.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer reader.Close()
	setAttachment(w, backup.Name)
	if backup.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(backup.Size, 10))
	}
	io.Copy(w, reader) //nolint:errcheck
}

func (a *app) handleDownloadDecrypted(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Key == "" {
		http.Error(w, "ENCRYPTION_KEY not configured", http.StatusPreconditionFailed)
		return
	}
	backup, err := a.lookupBackup(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client := a.getClient()
	if client == nil {
		http.Error(w, "Drive not connected", http.StatusServiceUnavailable)
		return
	}
	reader, err := client.Download(r.Context(), backup.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer reader.Close()

	dec, err := decryptor.NewDecryptingReader(reader, a.cfg.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer dec.Close()

	gz, err := gzip.NewReader(dec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer gz.Close()

	setAttachment(w, decryptedName(backup.Name))
	io.Copy(w, gz) //nolint:errcheck
}

// ── Scheduler ─────────────────────────────────────────────────────────────────

func (a *app) startScheduler(parentCtx context.Context) {
	schedCtx, cancel := context.WithCancel(parentCtx)
	a.schedulerCtx = cancel
	a.startSchedulerWithCtx(schedCtx)
}

func (a *app) startSchedulerWithCtx(ctx context.Context) {
	go func() {
		log.Println("scheduler started")
		for {
			a.scheduleMu.Lock()
			enabled := a.schedule.Enabled
			next := a.schedule.NextRun
			interval := time.Duration(a.schedule.IntervalSecs) * time.Second
			a.scheduleMu.Unlock()

			if !enabled || interval <= 0 {
				log.Println("scheduler disabled, exiting goroutine")
				return
			}

			wait := time.Until(next)
			if wait < 0 {
				wait = 0
			}

			select {
			case <-time.After(wait):
				log.Println("scheduler: triggering scheduled backup")
				backupCfg, err := a.cfg.toBackupConfig()
				if err != nil {
					log.Printf("scheduler: config error: %v", err)
				} else {
					job, err := a.startJob()
					if err != nil {
						log.Printf("scheduler: %v", err)
					} else {
						runErr := pipeline.RunWithLogger(context.Background(), backupCfg, job.appendLog)
						if runErr != nil {
							job.finish(jobFailed, runErr.Error())
							a.updateScheduleLastRun("failed")
						} else {
							job.finish(jobSuccess, "")
							a.updateScheduleLastRun("success")
						}
					}
				}
				a.scheduleMu.Lock()
				a.schedule.NextRun = time.Now().Add(interval)
				a.scheduleMu.Unlock()
				a.saveSchedule()
			case <-ctx.Done():
				log.Println("scheduler stopped")
				return
			}
		}
	}()
}

func (a *app) updateScheduleLastRun(status string) {
	a.scheduleMu.Lock()
	a.schedule.LastRun = time.Now()
	a.schedule.LastStatus = status
	a.scheduleMu.Unlock()
}

func (a *app) saveSchedule() {
	a.scheduleMu.Lock()
	data, _ := json.MarshalIndent(a.schedule, "", "  ")
	a.scheduleMu.Unlock()
	_ = os.WriteFile(scheduleFile, data, 0600)
}

func (a *app) loadSchedule() {
	data, err := os.ReadFile(scheduleFile)
	if err != nil {
		return
	}
	var sc scheduleConfig
	if err := json.Unmarshal(data, &sc); err == nil {
		a.schedule = sc
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (a *app) lookupBackup(r *http.Request) (uploader.BackupFile, error) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		return uploader.BackupFile{}, fmt.Errorf("missing id")
	}
	client := a.getClient()
	if client == nil {
		return uploader.BackupFile{}, fmt.Errorf("Drive not connected")
	}
	backups, err := client.ListBackups(r.Context(), a.cfg.Prefix)
	if err != nil {
		return uploader.BackupFile{}, err
	}
	for _, b := range backups {
		if b.ID == id {
			return b, nil
		}
	}
	return uploader.BackupFile{}, fmt.Errorf("backup not found")
}

func (a *app) render(w http.ResponseWriter, d viewData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "page", d); err != nil {
		log.Printf("render: %v", err)
	}
}

func setAttachment(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(name)))
	w.Header().Set("Cache-Control", "no-store")
}

func decryptedName(name string) string {
	base := strings.TrimSuffix(name, ".enc")
	base = strings.TrimSuffix(base, ".gz")
	base = strings.TrimSuffix(base, ".sql")
	if base == "" || base == "." {
		return "restore.dump"
	}
	if strings.HasSuffix(base, ".dump") {
		return base
	}
	return base + ".dump"
}

func humanSize(size int64) string {
	if size <= 0 {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(size)
	u := 0
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	if u == 0 {
		return fmt.Sprintf("%d B", size)
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(t).Round(time.Millisecond))
	})
}

// ── Template ──────────────────────────────────────────────────────────────────

func buildTemplates() *template.Template {
	return template.Must(template.New("page").Funcs(template.FuncMap{
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		"humanSize": humanSize,
		"inc":       func(v int) int { return v + 1 },
		"durFmt": func(d time.Duration) string {
			if d == 0 {
				return "—"
			}
			return d.Round(time.Second).String()
		},
		"safeHTML": func(s string) template.HTML { return template.HTML(s) },
	}).Parse(pageHTML))
}

const pageHTML = `{{define "page"}}<!DOCTYPEhtml>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>pgdrive — Backup Manager</title>
  <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700&display=swap">
  <link rel="stylesheet" href="https://fonts.googleapis.com/icon?family=Material+Icons+Round">
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    :root {
      --sw: 220px; --th: 60px;
      --acc: #2563eb; --acc-l: #eff6ff; --acc-d: #1d4ed8;
      --bg: #f3f4f6; --sur: #fff; --bdr: #e5e7eb;
      --tx: #111827; --txm: #6b7280; --txl: #9ca3af;
      --grn: #16a34a; --grn-bg: #dcfce7;
      --ylw: #b45309; --ylw-bg: #fef3c7;
      --red: #dc2626; --red-bg: #fee2e2;
      --rad: 8px; --shd: 0 1px 3px rgba(0,0,0,.08),0 1px 2px rgba(0,0,0,.04);
    }
    body { font-family:'Inter',-apple-system,BlinkMacSystemFont,sans-serif; background:var(--bg); color:var(--tx); display:flex; min-height:100vh; font-size:14px; }
    .sb { width:var(--sw); background:var(--sur); border-right:1px solid var(--bdr); display:flex; flex-direction:column; position:fixed; top:0; left:0; bottom:0; z-index:100; overflow-y:auto; }
    .sb-brand { display:flex; align-items:center; gap:10px; padding:16px 18px; border-bottom:1px solid var(--bdr); text-decoration:none; }
    .sb-icon { width:32px; height:32px; background:var(--acc); border-radius:8px; display:flex; align-items:center; justify-content:center; color:#fff; font-size:18px; flex-shrink:0; }
    .sb-title { font-size:15px; font-weight:700; color:var(--tx); }
    .sb-sub { font-size:11px; color:var(--txm); }
    .sb-sec { padding:12px 14px 4px; font-size:11px; font-weight:600; text-transform:uppercase; letter-spacing:.6px; color:var(--txl); }
    .sb-item { display:flex; align-items:center; gap:10px; padding:9px 12px; margin:1px 8px; border-radius:var(--rad); text-decoration:none; color:var(--txm); font-weight:500; font-size:13.5px; transition:background .15s,color .15s; }
    .sb-item:hover { background:var(--bg); color:var(--tx); }
    .sb-item.active { background:var(--acc-l); color:var(--acc-d); }
    .sb-item .mi { font-size:18px; }
    .sb-status { padding:6px 10px; margin:2px 8px; border-radius:var(--rad); font-size:12px; font-weight:500; display:flex; align-items:center; gap:6px; }
    .sb-status.ok { background:var(--grn-bg); color:var(--grn); }
    .sb-status.warn { background:var(--red-bg); color:var(--red); }
    .sb-foot { margin-top:auto; padding:14px 16px; border-top:1px solid var(--bdr); display:flex; align-items:center; gap:10px; }
    .sb-av { width:32px; height:32px; border-radius:50%; background:var(--acc); color:#fff; display:flex; align-items:center; justify-content:center; font-weight:700; font-size:12px; flex-shrink:0; }
    .layout { margin-left:var(--sw); flex:1; display:flex; flex-direction:column; min-height:100vh; }
    .tb { height:var(--th); background:var(--sur); border-bottom:1px solid var(--bdr); display:flex; align-items:center; padding:0 28px; gap:12px; position:sticky; top:0; z-index:50; }
    .tb-bc { display:flex; align-items:center; gap:6px; font-size:13px; color:var(--txm); }
    .tb-bc .sep { color:var(--txl); font-size:16px; }
    .tb-bc .cur { color:var(--tx); font-weight:600; }
    .tb-sp { flex:1; }
    .tb-btn { width:34px; height:34px; border:1px solid var(--bdr); border-radius:var(--rad); background:var(--sur); display:flex; align-items:center; justify-content:center; cursor:pointer; color:var(--txm); text-decoration:none; transition:background .15s; }
    .tb-btn:hover { background:var(--bg); }
    .pg { padding:28px; flex:1; }
    .pg-hdr { display:flex; align-items:flex-start; justify-content:space-between; gap:16px; margin-bottom:24px; flex-wrap:wrap; }
    .pg-ttl { font-size:22px; font-weight:700; }
    .pg-sub { font-size:13px; color:var(--txm); margin-top:3px; }
    .btn-row { display:flex; gap:8px; align-items:center; flex-wrap:wrap; }
    .btn { display:inline-flex; align-items:center; gap:6px; padding:8px 16px; border-radius:var(--rad); font-size:13.5px; font-weight:600; cursor:pointer; text-decoration:none; border:1px solid var(--bdr); background:var(--sur); color:var(--tx); transition:background .15s,border-color .15s; white-space:nowrap; font-family:inherit; }
    .btn:hover { background:var(--bg); }
    .btn .mi { font-size:16px; }
    .btn-primary { background:var(--acc); border-color:var(--acc); color:#fff; }
    .btn-primary:hover { background:var(--acc-d); border-color:var(--acc-d); }
    .btn-primary:disabled { opacity:.5; cursor:not-allowed; }
    .stats { display:grid; grid-template-columns:repeat(auto-fit,minmax(160px,1fr)); gap:16px; margin-bottom:24px; }
    .stat { background:var(--sur); border:1px solid var(--bdr); border-radius:var(--rad); padding:18px; box-shadow:var(--shd); }
    .stat-lbl { font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.5px; color:var(--txm); margin-bottom:8px; }
    .stat-val { font-size:24px; font-weight:700; }
    .stat-hint { font-size:12px; color:var(--txm); margin-top:4px; }
    .card { background:var(--sur); border:1px solid var(--bdr); border-radius:var(--rad); box-shadow:var(--shd); }
    .card-hdr { display:flex; align-items:center; gap:12px; padding:14px 20px; border-bottom:1px solid var(--bdr); flex-wrap:wrap; }
    .card-ttl { font-weight:600; font-size:15px; }
    .srch { display:flex; align-items:center; border:1px solid var(--bdr); border-radius:var(--rad); background:var(--bg); padding:0 12px; gap:8px; flex:1; min-width:180px; max-width:400px; }
    .srch .mi { color:var(--txl); font-size:18px; flex-shrink:0; }
    .srch input { border:none; background:transparent; outline:none; font-family:inherit; font-size:13.5px; color:var(--tx); padding:8px 0; width:100%; }
    .srch input::placeholder { color:var(--txl); }
    .tbl-wrap { overflow-x:auto; }
    table { width:100%; border-collapse:collapse; min-width:640px; }
    thead th { text-align:left; padding:10px 20px; font-size:12px; font-weight:600; text-transform:uppercase; letter-spacing:.4px; color:var(--txm); background:#fafafa; border-bottom:1px solid var(--bdr); }
    tbody tr { border-bottom:1px solid var(--bdr); transition:background .1s; }
    tbody tr:last-child { border-bottom:none; }
    tbody tr:hover { background:#fafafa; }
    td { padding:13px 20px; vertical-align:middle; }
    .bdg { display:inline-flex; align-items:center; padding:3px 10px; border-radius:99px; font-size:12px; font-weight:600; }
    .bdg-grn { background:var(--grn-bg); color:var(--grn); }
    .bdg-red { background:var(--red-bg); color:var(--red); }
    .bdg-ylw { background:var(--ylw-bg); color:var(--ylw); }
    .bdg-blu { background:var(--acc-l); color:var(--acc-d); }
    .bdg-gry { background:#f3f4f6; color:#6b7280; }
    .ac { display:flex; align-items:center; justify-content:flex-end; gap:8px; }
    .ib { display:inline-flex; align-items:center; justify-content:center; width:30px; height:30px; border-radius:6px; border:1px solid var(--bdr); background:var(--sur); color:var(--txm); cursor:pointer; text-decoration:none; transition:background .15s,color .15s; }
    .ib:hover { background:var(--bg); }
    .ib.blu:hover { color:var(--acc); border-color:#bfdbfe; background:var(--acc-l); }
    .ib.grn:hover { color:var(--grn); border-color:#bbf7d0; background:var(--grn-bg); }
    .ib .mi { font-size:16px; }
    .log-box { background:#0f172a; color:#e2e8f0; font-family:'Menlo','Monaco','Courier New',monospace; font-size:12.5px; padding:16px; border-radius:var(--rad); min-height:200px; max-height:420px; overflow-y:auto; line-height:1.6; white-space:pre-wrap; word-break:break-all; }
    .log-line-err { color:#f87171; }
    .log-line-info { color:#94a3b8; }
    .log-line-done { color:#facc15; }
    .form-row { display:grid; grid-template-columns:repeat(auto-fit,minmax(240px,1fr)); gap:16px; margin-bottom:16px; }
    .form-group { display:flex; flex-direction:column; gap:6px; }
    .form-group label { font-size:12px; font-weight:600; color:var(--txm); text-transform:uppercase; letter-spacing:.4px; }
    .form-group input, .form-group select { border:1px solid var(--bdr); border-radius:var(--rad); padding:9px 12px; font-size:14px; font-family:inherit; color:var(--tx); background:var(--sur); outline:none; transition:border-color .15s; }
    .form-group input:focus, .form-group select:focus { border-color:var(--acc); }
    .form-group input[readonly] { background:#fafafa; color:var(--txm); }
    .form-hint { font-size:12px; color:var(--txl); }
    .toggle-wrap { display:flex; align-items:center; gap:10px; }
    .toggle { position:relative; display:inline-block; width:44px; height:24px; cursor:pointer; }
    .toggle input { opacity:0; width:0; height:0; }
    .toggle-slider { position:absolute; inset:0; background:#d1d5db; border-radius:24px; transition:.3s; }
    .toggle-slider::before { content:''; position:absolute; height:18px; width:18px; left:3px; bottom:3px; background:#fff; border-radius:50%; transition:.3s; }
    .toggle input:checked + .toggle-slider { background:var(--acc); }
    .toggle input:checked + .toggle-slider::before { transform:translateX(20px); }
    .info-note { display:flex; align-items:flex-start; gap:10px; padding:14px 20px; background:var(--acc-l); border-top:1px solid #bfdbfe; font-size:13px; color:var(--acc-d); }
    .info-note .mi { font-size:16px; margin-top:1px; flex-shrink:0; }
    code { background:rgba(37,99,235,.1); padding:1px 5px; border-radius:4px; font-family:monospace; font-size:12px; }
    .flash { display:flex; align-items:center; gap:10px; padding:12px 16px; border-radius:var(--rad); margin-bottom:20px; font-size:13.5px; font-weight:500; }
    .flash-ok { background:var(--grn-bg); color:var(--grn); border:1px solid #bbf7d0; }
    .flash-err { background:var(--red-bg); color:var(--red); border:1px solid #fca5a5; }
    .empty { text-align:center; padding:56px 20px; color:var(--txm); }
    .empty .mi { font-size:44px; color:var(--txl); display:block; margin-bottom:10px; }
    @keyframes spin { to { transform:rotate(360deg); } }
    .spin { animation:spin .8s linear infinite; display:inline-block; }
    .oauth-card { max-width:520px; }
    .oauth-step { display:flex; align-items:flex-start; gap:14px; padding:16px 0; border-bottom:1px solid var(--bdr); }
    .oauth-step:last-child { border-bottom:none; }
    .oauth-num { width:28px; height:28px; border-radius:50%; background:var(--acc); color:#fff; display:flex; align-items:center; justify-content:center; font-size:13px; font-weight:700; flex-shrink:0; margin-top:1px; }
    .oauth-step-txt h4 { font-size:14px; font-weight:600; margin-bottom:4px; }
    .oauth-step-txt p { font-size:13px; color:var(--txm); line-height:1.5; }
    @media(max-width:768px) { .sb { display:none; } .layout { margin-left:0; } .pg { padding:16px; } }
  </style>
</head>
<body>
<aside class="sb">
  <a href="/" class="sb-brand">
    <div class="sb-icon"><span class="mi material-icons-round">backup</span></div>
    <div><div class="sb-title">pgdrive</div><div class="sb-sub">Backup Manager</div></div>
  </a>
  <div class="sb-sec">Management</div>
  <a href="/backups"  class="sb-item{{if eq .Page "backups"}} active{{end}}"><span class="mi material-icons-round">cloud_download</span> Backups</a>
  <a href="/run"      class="sb-item{{if eq .Page "run"}} active{{end}}"><span class="mi material-icons-round">play_circle</span> Run Backup</a>
  <a href="/schedule" class="sb-item{{if eq .Page "schedule"}} active{{end}}"><span class="mi material-icons-round">schedule</span> Schedule</a>
  <a href="/logs"     class="sb-item{{if eq .Page "logs"}} active{{end}}"><span class="mi material-icons-round">history</span> Logs</a>
  <div class="sb-sec" style="margin-top:8px;">System</div>
  <a href="/settings" class="sb-item{{if eq .Page "settings"}} active{{end}}"><span class="mi material-icons-round">tune</span> Settings</a>
  <a href="/oauth"    class="sb-item{{if eq .Page "oauth"}} active{{end}}"><span class="mi material-icons-round">cloud_sync</span> Google Drive</a>
  <div style="padding:8px 14px 4px;">
    {{if .HasToken}}<div class="sb-status ok"><span class="mi material-icons-round" style="font-size:16px;">check_circle</span> Drive connected</div>
    {{else}}<div class="sb-status warn"><span class="mi material-icons-round" style="font-size:16px;">warning</span> Drive not auth'd</div>{{end}}
    {{if .HasKey}}<div class="sb-status ok" style="margin-top:4px;"><span class="mi material-icons-round" style="font-size:16px;">lock_open</span> Decrypt ready</div>
    {{else}}<div class="sb-status warn" style="margin-top:4px;"><span class="mi material-icons-round" style="font-size:16px;">lock</span> No encryption key</div>{{end}}
  </div>
  <div class="sb-foot">
    <div class="sb-av">PG</div>
    <div><div style="font-size:13px;font-weight:600;">pgdrive</div><div style="font-size:11px;color:var(--txm);">Admin</div></div>
  </div>
</aside>
<div class="layout">
  <header class="tb">
    <nav class="tb-bc">
      <span>pgdrive</span>
      <span class="mi material-icons-round sep" style="font-size:16px;">chevron_right</span>
      <span class="cur">
        {{if eq .Page "backups"}}Backups
        {{else if eq .Page "run"}}Run Backup
        {{else if eq .Page "schedule"}}Schedule
        {{else if eq .Page "logs"}}Logs
        {{else if eq .Page "settings"}}Settings
        {{else if eq .Page "oauth"}}Google Drive Setup
        {{end}}
      </span>
    </nav>
    <div class="tb-sp"></div>
    <a href="/backups" class="tb-btn" title="Backups"><span class="mi material-icons-round">cloud_download</span></a>
    <a href="/run" class="tb-btn" title="Run backup"><span class="mi material-icons-round">play_circle</span></a>
  </header>
  <main class="pg">
    {{if .Flash}}<div class="flash flash-ok"><span class="mi material-icons-round">check_circle</span> {{.Flash}}</div>{{end}}
    {{if .Error}}<div class="flash flash-err"><span class="mi material-icons-round">error_outline</span> {{.Error}}</div>{{end}}

    {{if eq .Page "backups"}}
    <div class="pg-hdr">
      <div><div class="pg-ttl">Backups</div><div class="pg-sub">Browse, download and restore your PostgreSQL Drive backups</div></div>
      <div class="btn-row">
        <a href="/backups" class="btn"><span class="mi material-icons-round">refresh</span> Refresh</a>
        <a href="/run" class="btn btn-primary"><span class="mi material-icons-round">play_circle</span> Run Backup</a>
      </div>
    </div>
    <div class="stats">
      <div class="stat"><div class="stat-lbl">Total Backups</div><div class="stat-val">{{len .Backups}}</div></div>
      <div class="stat"><div class="stat-lbl">Latest Backup</div><div class="stat-val" style="font-size:14px;padding-top:6px;">{{if .Latest}}{{formatTime .Latest.CreatedTime}}{{else}}—{{end}}</div></div>
      <div class="stat"><div class="stat-lbl">Latest Size</div><div class="stat-val" style="font-size:20px;padding-top:4px;">{{if .Latest}}{{humanSize .Latest.Size}}{{else}}—{{end}}</div></div>
      <div class="stat"><div class="stat-lbl">Prefix</div><div class="stat-val" style="font-size:18px;padding-top:4px;">{{if .Prefix}}{{.Prefix}}{{else}}All{{end}}</div></div>
    </div>
    <div class="card">
      <div class="card-hdr">
        <div class="srch"><span class="mi material-icons-round">search</span><input type="text" id="srch" placeholder="Search backups..."></div>
        <div style="margin-left:auto;font-size:13px;color:var(--txm);"><span id="cnt">{{len .Backups}}</span> backups</div>
      </div>
      <div class="tbl-wrap">
        <table>
          <thead><tr><th style="width:48px;">#</th><th>Backup File</th><th>Created</th><th>Size</th><th>Status</th><th style="text-align:right;">Actions</th></tr></thead>
          <tbody id="tbl">
            {{range $i, $b := .Backups}}
            <tr data-name="{{$b.Name}}">
              <td style="color:var(--txl);font-size:13px;">{{$i | inc}}</td>
              <td><div style="font-weight:600;margin-bottom:3px;">{{$b.Name}}</div><div style="font-family:monospace;font-size:11.5px;color:var(--txm);">{{$b.ID}}</div></td>
              <td style="color:var(--txm);font-size:13px;white-space:nowrap;">{{formatTime $b.CreatedTime}}</td>
              <td><span class="bdg bdg-gry">{{humanSize $b.Size}}</span></td>
              <td><span class="bdg bdg-grn">active</span>{{if $.HasKey}} <span class="bdg bdg-blu">decrypt ready</span>{{end}}</td>
              <td><div class="ac">
					 <a href="/download?id={{$b.ID}}" class="ib blu" title="Download encrypted" onclick="return confirm('Are you sure you want to download this encrypted backup?');"><span class="mi material-icons-round">file_download</span></a>
					 {{if $.HasKey}}<a href="/decrypt?id={{$b.ID}}" class="ib grn" title="Download decrypted" onclick="return confirm('Are you sure you want to download and decrypt this backup?');"><span class="mi material-icons-round">lock_open</span></a>{{end}}
              </div></td>
            </tr>
            {{end}}
          </tbody>
        </table>
        {{if not .Backups}}<div class="empty"><span class="mi material-icons-round">cloud_off</span><p>No backups found.</p></div>{{end}}
      </div>
      <div class="info-note"><span class="mi material-icons-round">info</span><div>Decrypted downloads stream via AES-256-CTR + gzip decompression. Output is a <code>pg_dump --format=custom</code> archive.</div></div>
    </div>
    {{end}}

    {{if eq .Page "run"}}
    <div class="pg-hdr">
      <div><div class="pg-ttl">Run Backup</div><div class="pg-sub">Trigger a manual backup now and watch it live</div></div>
    </div>
    <div class="stats">
      <div class="stat"><div class="stat-lbl">Database</div><div class="stat-val" style="font-size:16px;padding-top:4px;">{{if .Settings.PGDatabase}}{{.Settings.PGDatabase}}{{else}}<span style="color:var(--red)">Not set</span>{{end}}</div></div>
      <div class="stat"><div class="stat-lbl">Drive</div><div class="stat-val" style="font-size:16px;padding-top:4px;">{{if .HasToken}}<span style="color:var(--grn)">Connected</span>{{else}}<span style="color:var(--red)">Not connected</span>{{end}}</div></div>
      <div class="stat"><div class="stat-lbl">Encryption Key</div><div class="stat-val" style="font-size:16px;padding-top:4px;">{{if .HasKey}}<span style="color:var(--grn)">Configured</span>{{else}}<span style="color:var(--red)">Missing</span>{{end}}</div></div>
    </div>
    <div class="card" style="margin-bottom:20px;">
      <div class="card-hdr">
        <span class="mi material-icons-round" style="color:var(--acc);">play_circle</span>
        <span class="card-ttl">Manual Backup</span>
        <div style="margin-left:auto;" id="job-status-badge"></div>
      </div>
      <div style="padding:20px;">
        <p style="color:var(--txm);margin-bottom:16px;font-size:13.5px;">Runs a full <strong>pg_dump → gzip → AES-256 encrypt → Google Drive upload</strong> pipeline.</p>
        <button class="btn btn-primary" id="run-btn" onclick="startBackup()"><span class="mi material-icons-round">play_arrow</span> Run Backup Now</button>
        <button class="btn" id="cancel-btn" style="display:none;margin-left:8px;" onclick="cancelBackup()"><span class="mi material-icons-round">stop</span> Cancel</button>
      </div>
    </div>
    <div class="card">
      <div class="card-hdr">
        <span class="mi material-icons-round" style="color:var(--txm);">terminal</span>
        <span class="card-ttl">Live Output</span>
        <div style="margin-left:auto;font-size:12px;color:var(--txm);" id="log-meta">Waiting to start...</div>
      </div>
      <div style="padding:16px;"><div class="log-box" id="log-box">Ready. Click "Run Backup Now" to start.</div></div>
    </div>
    <script>
    let es = null, startTime = null;
    const logBox = document.getElementById('log-box');
    const runBtn = document.getElementById('run-btn');
    const cancelBtn = document.getElementById('cancel-btn');
    const logMeta = document.getElementById('log-meta');
    const statusBadge = document.getElementById('job-status-badge');
    function appendLog(line) {
      const d = document.createElement('div');
      d.className = (line.includes('error')||line.includes('Error')||line.includes('failed')) ? 'log-line-err' : (line.startsWith('──') ? 'log-line-done' : 'log-line-info');
      d.textContent = '[' + new Date().toLocaleTimeString() + '] ' + line;
      logBox.appendChild(d); logBox.scrollTop = logBox.scrollHeight;
    }
    function setStatus(s) {
      statusBadge.innerHTML = s==='running'?'<span class="bdg bdg-ylw">&#9679; Running...</span>':s==='success'?'<span class="bdg bdg-grn">&#10003; Success</span>':'<span class="bdg bdg-red">&#10005; Failed</span>';
    }
    function startBackup() {
      logBox.innerHTML = ''; appendLog('Starting backup...'); runBtn.disabled = true; cancelBtn.style.display = 'inline-flex'; startTime = Date.now(); setStatus('running');
      fetch('/api/backup/run',{method:'POST'}).then(r=>r.json()).then(d=>{
        if(d.error){appendLog('Error: '+d.error);setStatus('failed');reset();return;}
        appendLog('Job #'+d.job_id+' started'); streamLogs();
      }).catch(e=>{appendLog('Network error: '+e);setStatus('failed');reset();});
    }
    function streamLogs() {
      if(es)es.close();
      es = new EventSource('/api/backup/stream');
      es.onmessage = function(e){
        const data = JSON.parse(e.data);
        if(data.log) appendLog(data.log);
        if(data.done){ es.close();es=null; setStatus(data.status); logMeta.textContent='Finished in '+((Date.now()-startTime)/1000).toFixed(1)+'s'; appendLog('── job '+data.status+' ──'); reset(); }
      };
      es.onerror = function(){appendLog('Stream disconnected.');es.close();es=null;reset();};
    }
    function cancelBackup(){if(es){es.close();es=null;}reset();appendLog('Cancelled.');}
    function reset(){runBtn.disabled=false;cancelBtn.style.display='none';}
    fetch('/api/backup/status').then(r=>r.json()).then(d=>{
      if(d.status==='running'){runBtn.disabled=true;cancelBtn.style.display='inline-flex';setStatus('running');logBox.innerHTML='';(d.logs||[]).forEach(appendLog);streamLogs();}
    });
    </script>
    {{end}}

    {{if eq .Page "schedule"}}
    <div class="pg-hdr">
      <div><div class="pg-ttl">Schedule</div><div class="pg-sub">Configure automatic backup intervals</div></div>
    </div>
    <div class="card" style="max-width:600px;">
      <div class="card-hdr"><span class="mi material-icons-round" style="color:var(--acc);">schedule</span><span class="card-ttl">Backup Schedule</span></div>
      <form action="/api/schedule/save" method="POST" style="padding:24px;">
        <div style="margin-bottom:24px;">
          <div class="toggle-wrap">
            <label class="toggle"><input type="checkbox" id="sch-en" name="enabled" value="1"{{if .Schedule.Enabled}} checked{{end}} onchange="toggleSchedule(this)"><span class="toggle-slider"></span></label>
            <span style="font-weight:600;font-size:14px;" id="sch-en-lbl">{{if .Schedule.Enabled}}Enabled{{else}}Disabled{{end}}</span>
          </div>
          <div class="form-hint" style="margin-top:6px;">When enabled, backups run automatically at the configured interval.</div>
        </div>
        <div id="sch-fields" style="{{if not .Schedule.Enabled}}opacity:.5;pointer-events:none;{{end}}">
          <div class="form-row">
            <div class="form-group">
              <label>Interval</label>
              <select name="interval" id="sch-interval">
                <option value="1h"   {{if eq .Schedule.IntervalSecs 3600   }}selected{{end}}>Every hour</option>
                <option value="6h"   {{if eq .Schedule.IntervalSecs 21600  }}selected{{end}}>Every 6 hours</option>
                <option value="12h"  {{if eq .Schedule.IntervalSecs 43200  }}selected{{end}}>Every 12 hours</option>
                <option value="24h"  {{if eq .Schedule.IntervalSecs 86400  }}selected{{end}}>Daily (every 24h)</option>
                <option value="168h" {{if eq .Schedule.IntervalSecs 604800 }}selected{{end}}>Weekly</option>
              </select>
            </div>
          </div>
          {{if not .Schedule.NextRun.IsZero}}<div class="form-hint" style="margin-bottom:12px;">Next run: <strong>{{formatTime .Schedule.NextRun}}</strong></div>{{end}}
          {{if not .Schedule.LastRun.IsZero}}<div class="form-hint" style="margin-bottom:16px;">Last run: <strong>{{formatTime .Schedule.LastRun}}</strong>{{if .Schedule.LastStatus}} — <span class="bdg {{if eq .Schedule.LastStatus "success"}}bdg-grn{{else}}bdg-red{{end}}">{{.Schedule.LastStatus}}</span>{{end}}</div>{{end}}
        </div>
        <div style="display:flex;gap:10px;margin-top:4px;">
          <button type="submit" class="btn btn-primary"><span class="mi material-icons-round">save</span> Save Schedule</button>
          <a href="/schedule" class="btn">Cancel</a>
        </div>
      </form>
    </div>
    <script>
    function toggleSchedule(cb){
      document.getElementById('sch-en-lbl').textContent=cb.checked?'Enabled':'Disabled';
      const f=document.getElementById('sch-fields');f.style.opacity=cb.checked?'1':'.5';f.style.pointerEvents=cb.checked?'auto':'none';
    }
    </script>
    {{end}}

    {{if eq .Page "logs"}}
    <div class="pg-hdr">
      <div><div class="pg-ttl">Job History</div><div class="pg-sub">Past and current backup runs (last 50)</div></div>
      <a href="/run" class="btn btn-primary"><span class="mi material-icons-round">play_circle</span> Run Now</a>
    </div>
    <div class="card">
      <div class="tbl-wrap">
        <table>
          <thead><tr><th style="width:60px;">#</th><th>Started</th><th>Finished</th><th>Duration</th><th>Status</th><th>Detail</th></tr></thead>
          <tbody>
            {{range .Jobs}}
            <tr>
              <td style="font-size:13px;color:var(--txl);">{{.ID}}</td>
              <td style="font-size:13px;color:var(--txm);">{{formatTime .StartedAt}}</td>
              <td style="font-size:13px;color:var(--txm);">{{if not .FinishedAt.IsZero}}{{formatTime .FinishedAt}}{{else}}—{{end}}</td>
              <td><span class="bdg bdg-gry">{{durFmt .Duration}}</span></td>
              <td>{{if eq .Status "running"}}<span class="bdg bdg-ylw"><span class="spin">&#9679;</span> Running</span>
              {{else if eq .Status "success"}}<span class="bdg bdg-grn">&#10003; Success</span>
              {{else if eq .Status "failed"}}<span class="bdg bdg-red">&#10005; Failed</span>
              {{else}}<span class="bdg bdg-gry">—</span>{{end}}</td>
              <td style="max-width:300px;">{{if .ErrMsg}}<span style="font-size:12px;color:var(--red);">{{.ErrMsg}}</span>{{else}}<span style="color:var(--txl);font-size:12px;">—</span>{{end}}</td>
            </tr>
            {{end}}
          </tbody>
        </table>
        {{if not .Jobs}}<div class="empty"><span class="mi material-icons-round">history</span><p>No jobs run yet. <a href="/run" style="color:var(--acc);">Run a backup</a> to see history here.</p></div>{{end}}
      </div>
    </div>
    {{end}}

    {{if eq .Page "settings"}}
    <div class="pg-hdr">
      <div><div class="pg-ttl">Settings</div><div class="pg-sub">Configure your backup settings — saved to disk and applied immediately</div></div>
    </div>
    <form action="/api/settings/save" method="POST">
    <div class="card" style="max-width:720px;margin-bottom:16px;">
      <div class="card-hdr"><span class="mi material-icons-round" style="color:var(--acc);">storage</span><span class="card-ttl">PostgreSQL</span></div>
      <div style="padding:20px;">
        <div class="form-row">
          <div class="form-group"><label>PG Host</label><input type="text" name="pg_host" value="{{.Settings.PGHost}}" placeholder="localhost"></div>
          <div class="form-group"><label>PG Port</label><input type="text" name="pg_port" value="{{.Settings.PGPort}}" placeholder="5432"></div>
        </div>
        <div class="form-row">
          <div class="form-group"><label>PG User</label><input type="text" name="pg_user" value="{{.Settings.PGUser}}" placeholder="postgres"></div>
          <div class="form-group"><label>PG Database</label><input type="text" name="pg_database" value="{{.Settings.PGDatabase}}" placeholder="mydb"></div>
        </div>
        <div class="form-row">
          <div class="form-group"><label>PG Password</label><input type="password" name="pg_password" value="{{.Settings.PGPassword}}" placeholder="••••••••"></div>
        </div>
      </div>
    </div>
    <div class="card" style="max-width:720px;margin-bottom:16px;">
      <div class="card-hdr"><span class="mi material-icons-round" style="color:var(--acc);">cloud</span><span class="card-ttl">Google Drive</span></div>
      <div style="padding:20px;">
        <div class="form-row">
          <div class="form-group"><label>Drive Folder ID</label><input type="text" name="folder_id" value="{{.Settings.FolderID}}" placeholder="1A2B3C4D5E..."></div>
          <div class="form-group"><label>Backup Prefix</label><input type="text" name="prefix" value="{{.Settings.Prefix}}" placeholder="pgbackup"></div>
        </div>
        <div class="form-row">
          <div class="form-group"><label>Credentials File</label><input type="text" name="credentials" value="{{.Settings.Credentials}}" placeholder="credentials.json"></div>
          <div class="form-group"><label>Token File</label><input type="text" name="token_file" value="{{.Settings.TokenFile}}" placeholder="token.json"></div>
        </div>
        <div style="margin-top:8px;"><a href="/oauth" class="btn"><span class="mi material-icons-round">cloud_sync</span> Manage Google Drive Auth</a></div>
      </div>
    </div>
    <div class="card" style="max-width:720px;margin-bottom:16px;">
      <div class="card-hdr"><span class="mi material-icons-round" style="color:var(--acc);">security</span><span class="card-ttl">Backup</span></div>
      <div style="padding:20px;">
        <div class="form-row">
          <div class="form-group"><label>Encryption Key (64 hex chars)</label><input type="password" name="key" value="" placeholder="Leave blank to keep current key"><div class="form-hint">{{if .HasKey}}Key is set. Enter a new value to replace it.{{else}}No key set — backups will fail without one.{{end}}</div></div>
          <div class="form-group"><label>Retention Days</label><input type="number" name="retention_days" value="{{.Settings.RetentionDays}}" min="1" placeholder="7"></div>
        </div>
      </div>
    </div>
    <div style="max-width:720px;display:flex;gap:10px;margin-top:4px;">
      <button type="submit" class="btn btn-primary"><span class="mi material-icons-round">save</span> Save Settings</button>
      <a href="/settings" class="btn">Reset</a>
    </div>
    </form>
    {{end}}

    {{if eq .Page "oauth"}}
    <div class="pg-hdr">
      <div><div class="pg-ttl">Google Drive Setup</div><div class="pg-sub">Authorize pgdrive to access your Google Drive</div></div>
    </div>
    <div class="card oauth-card" style="margin-bottom:20px;">
      <div class="card-hdr">
        <span class="mi material-icons-round" style="color:var(--acc);">cloud_sync</span>
        <span class="card-ttl">Connection Status</span>
        <div style="margin-left:auto;">{{if .HasToken}}<span class="bdg bdg-grn">&#10003; Connected</span>{{else}}<span class="bdg bdg-red">&#10005; Not connected</span>{{end}}</div>
      </div>
      <div style="padding:20px 24px;">
        <div class="oauth-step">
          <div class="oauth-num">1</div>
          <div class="oauth-step-txt">
            <h4>Add redirect URI to Google Cloud Console</h4>
            <p>In your Google Cloud project go to <strong>Credentials → OAuth 2.0 Client IDs</strong> and add as an authorized redirect URI:</p>
            <code style="display:block;margin-top:8px;padding:8px 12px;background:#f3f4f6;border-radius:6px;font-size:13px;">http://localhost:8080/oauth/callback</code>
          </div>
        </div>
        <div class="oauth-step">
          <div class="oauth-num">2</div>
          <div class="oauth-step-txt">
            <h4>Authorize with Google</h4>
            <p>Click the button below to open the Google authorization page.</p>
            <div style="margin-top:12px;">
              {{if .OAuthURL}}<a href="{{.OAuthURL}}" class="btn btn-primary"><span class="mi material-icons-round">login</span> Connect Google Drive</a>
              {{else}}<div class="flash flash-err" style="margin-top:0;"><span class="mi material-icons-round">error</span> credentials.json not found or invalid.</div>{{end}}
            </div>
          </div>
        </div>
        <div class="oauth-step">
          <div class="oauth-num">3</div>
          <div class="oauth-step-txt">
            <h4>Token saved automatically</h4>
            <p>After authorization, your token is saved to <code>{{.Settings.TokenFile}}</code>. Subsequent restarts connect automatically.</p>
          </div>
        </div>
      </div>
    </div>
    {{if .HasToken}}
    <div class="card oauth-card">
      <div class="card-hdr"><span class="mi material-icons-round" style="color:var(--grn);">check_circle</span><span class="card-ttl">Re-authorize</span></div>
      <div style="padding:20px 24px;">
        <p style="font-size:13.5px;color:var(--txm);margin-bottom:16px;">If your token expired or you want to switch accounts, re-authorize below.</p>
        {{if .OAuthURL}}<a href="{{.OAuthURL}}" class="btn"><span class="mi material-icons-round">refresh</span> Re-authorize Google Drive</a>{{end}}
      </div>
    </div>
    {{end}}
    {{end}}

  </main>
</div>
<script>
const srch = document.getElementById('srch');
const tbl = document.getElementById('tbl');
const cnt = document.getElementById('cnt');
if (srch && tbl) {
  const rows = Array.from(tbl.querySelectorAll('tr[data-name]'));
  srch.addEventListener('input', () => {
    const q = srch.value.trim().toLowerCase();
    let v = 0;
    rows.forEach(r => { const m = q===''||r.dataset.name.toLowerCase().includes(q); r.style.display=m?'':'none'; if(m)v++; });
    if(cnt) cnt.textContent = v;
  });
}
</script>
</body>
</html>{{end}}`
