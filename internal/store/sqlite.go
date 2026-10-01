// Package store persists projects, immutable asset records, revisions and render jobs.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zylar06/video-agent/internal/catalog"
	"github.com/zylar06/video-agent/internal/domain"
	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("revision conflict")
var ErrNotFound = errors.New("not found")
var ErrOperationReuse = errors.New("operation id reused with different payload")

type Store struct {
	db  *sql.DB
	Dir string
}

func Open(dir string) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// The leading slash is required: on Windows a path of "E:/dir/db" renders as
	// "file://E:/dir/db", where "E:" is parsed as the URL host and SQLite fails
	// with "out of memory". "file:///E:/dir/db" is the correct form and stays
	// "file:///tmp/..." on Unix, so both platforms work.
	u := url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(filepath.Join(dir, "video-agent.db"))}
	db, err := sql.Open("sqlite", u.String()+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > 5 {
		return fail(errors.New("database is newer than this application"))
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY, body BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS assets(project_id TEXT NOT NULL REFERENCES projects(id), id TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(project_id,id));
CREATE TABLE IF NOT EXISTS timelines(id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id), revision INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS revisions(timeline_id TEXT NOT NULL REFERENCES timelines(id), revision INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(timeline_id,revision));
CREATE TABLE IF NOT EXISTS operations(timeline_id TEXT NOT NULL, id TEXT NOT NULL, payload BLOB NOT NULL, revision INTEGER NOT NULL, PRIMARY KEY(timeline_id,id), FOREIGN KEY(timeline_id,revision) REFERENCES revisions(timeline_id,revision));
CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY, timeline_id TEXT NOT NULL, revision INTEGER NOT NULL, status TEXT NOT NULL, body BLOB NOT NULL, FOREIGN KEY(timeline_id,revision) REFERENCES revisions(timeline_id,revision));`)
	if err != nil {
		return fail(err)
	}
	if version < 2 {
		_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS evidence(project_id TEXT NOT NULL REFERENCES projects(id), asset_id TEXT NOT NULL, id TEXT NOT NULL, cache_key TEXT NOT NULL DEFAULT '', body BLOB NOT NULL, PRIMARY KEY(project_id,id), FOREIGN KEY(project_id,asset_id) REFERENCES assets(project_id,id));
CREATE INDEX IF NOT EXISTS evidence_project_asset ON evidence(project_id,asset_id);
CREATE INDEX IF NOT EXISTS evidence_project_cache ON evidence(project_id,cache_key);
PRAGMA user_version=2;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 3 {
		_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS analysis_runs(project_id TEXT NOT NULL REFERENCES projects(id), asset_id TEXT NOT NULL, cache_key TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(project_id,asset_id,cache_key), FOREIGN KEY(project_id,asset_id) REFERENCES assets(project_id,id));
CREATE INDEX IF NOT EXISTS analysis_runs_asset ON analysis_runs(project_id,asset_id);
PRAGMA user_version=3;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 4 {
		_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS draft_plans(id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id), asset_id TEXT NOT NULL, version INTEGER NOT NULL, body BLOB NOT NULL, FOREIGN KEY(project_id,asset_id) REFERENCES assets(project_id, id));
CREATE INDEX IF NOT EXISTS draft_plans_project_asset ON draft_plans(project_id,asset_id);
PRAGMA user_version=4;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 5 {
		// Conversations and UI analysis tasks used to live only in process memory,
		// so a restart silently discarded both. They are stored as JSON bodies
		// because the only reader is the service that wrote them.
		_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, preview TEXT NOT NULL DEFAULT '', messages INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL, body BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions(updated_at DESC);
CREATE TABLE IF NOT EXISTS ui_tasks(id TEXT PRIMARY KEY, project_id TEXT NOT NULL, asset_id TEXT NOT NULL, status TEXT NOT NULL, body BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS ui_tasks_status ON ui_tasks(status);
PRAGMA user_version=5;`)
		if err != nil {
			return fail(err)
		}
	}
	return &Store{db: db, Dir: dir}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func decode(row *sql.Row, out any) error {
	var b []byte
	if err := row.Scan(&b); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return json.Unmarshal(b, out)
}

func (s *Store) CreateProject(p domain.Project) error {
	if p.ID == "" || p.Name == "" {
		return errors.New("project requires id and name")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO projects VALUES(?,?)", p.ID, b)
	return err
}
func (s *Store) Project(id string) (p domain.Project, err error) {
	err = decode(s.db.QueryRow("SELECT body FROM projects WHERE id=?", id), &p)
	return
}

// Projects lists every project. Without this an agent has no way to discover
// existing work: it can only guess ids, which reads to a user as the assistant
// having forgotten everything.
func (s *Store) Projects() ([]domain.Project, error) {
	rows, err := s.db.Query("SELECT body FROM projects ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []domain.Project{}
	for rows.Next() {
		var b []byte
		var p domain.Project
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
func (s *Store) PutAsset(a domain.MediaAsset) (domain.MediaAsset, error) {
	if err := a.Validate(); err != nil {
		return a, err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return a, err
	}
	_, err = s.db.Exec("INSERT INTO assets VALUES(?,?,?) ON CONFLICT(project_id,id) DO NOTHING", a.ProjectID, a.ID, b)
	if err != nil {
		return a, err
	}
	return s.Asset(a.ProjectID, a.ID)
}
func (s *Store) Asset(project, id string) (a domain.MediaAsset, err error) {
	err = decode(s.db.QueryRow("SELECT body FROM assets WHERE project_id=? AND id=?", project, id), &a)
	return
}
func (s *Store) Assets(project string) (map[string]domain.MediaAsset, error) {
	rows, err := s.db.Query("SELECT body FROM assets WHERE project_id=? ORDER BY id", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := map[string]domain.MediaAsset{}
	for rows.Next() {
		var b []byte
		var a domain.MediaAsset
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		assets[a.ID] = a
	}
	return assets, rows.Err()
}

// PutEvidence is idempotent for an identical ID and refuses an accidental
// cross-project/cache overwrite. Evidence never bypasses imported asset bounds.
func (s *Store) PutEvidence(e domain.Evidence) (domain.Evidence, error) {
	asset, err := s.Asset(e.ProjectID, e.AssetID)
	if err != nil {
		return e, err
	}
	if e.AssetContentHash == "" {
		e.AssetContentHash = asset.ContentHash
	}
	if err = e.Validate(asset); err != nil {
		return e, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	var existing []byte
	err = s.db.QueryRow("SELECT body FROM evidence WHERE project_id=? AND id=?", e.ProjectID, e.ID).Scan(&existing)
	if err == nil {
		if string(existing) != string(b) {
			return e, ErrOperationReuse
		}
		return e, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return e, err
	}
	_, err = s.db.Exec("INSERT INTO evidence(project_id,asset_id,id,cache_key,body) VALUES(?,?,?,?,?)", e.ProjectID, e.AssetID, e.ID, e.CacheKey, b)
	return e, err
}

func (s *Store) Evidence(project string, assetIDs []string) ([]domain.Evidence, error) {
	if _, err := s.Project(project); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT body FROM evidence WHERE project_id=? ORDER BY id", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allowed := map[string]bool{}
	for _, id := range assetIDs {
		allowed[id] = true
	}
	out := []domain.Evidence{}
	for rows.Next() {
		var b []byte
		var e domain.Evidence
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		if len(allowed) == 0 || allowed[e.AssetID] {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// CurrentEvidence isolates the latest completed analysis from historical runs.
func (s *Store) CurrentEvidence(project, asset string) ([]domain.Evidence, error) {
	items, err := s.Evidence(project, []string{asset})
	if err != nil {
		return nil, err
	}
	runs, err := s.AnalysisRuns(project, asset)
	if err != nil {
		return nil, err
	}
	var latest domain.AnalysisRun
	for _, r := range runs {
		if r.Status == "completed" && r.UpdatedAt.After(latest.UpdatedAt) {
			latest = r
		}
	}
	if latest.CacheKey == "" {
		return items, nil
	}
	allowed := map[string]bool{}
	for _, id := range latest.EvidenceIDs {
		allowed[id] = true
	}
	out := []domain.Evidence{}
	for _, e := range items {
		if allowed[e.ID] {
			out = append(out, e)
		}
	}
	return out, nil
}

// SearchEvidence is a deterministic offline lexical search. It is deliberately
// transparent: P2 can replace scoring without changing P3's source contracts.
func (s *Store) SearchEvidence(project, query string, assetIDs []string, limit int) ([]catalog.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("search query is required")
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("search limit must be 1..100")
	}
	evidence, err := s.Evidence(project, assetIDs)
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(query))
	out := make([]catalog.SearchResult, 0, len(evidence))
	for _, e := range evidence {
		text := strings.ToLower(e.Transcript + " " + e.VisualSummary)
		matches := 0
		for _, term := range terms {
			if strings.Contains(text, term) {
				matches++
			}
		}
		if matches == 0 {
			continue
		}
		out = append(out, catalog.SearchResult{Evidence: e, Score: float64(matches) / float64(len(terms)), Reason: "matched evidence transcript or visual_summary"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Evidence.ID < out[j].Evidence.ID
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) PutAnalysisRun(run domain.AnalysisRun) (domain.AnalysisRun, error) {
	asset, err := s.Asset(run.ProjectID, run.AssetID)
	if err != nil {
		return run, err
	}
	if run.CacheKey == "" || run.AssetContentHash != asset.ContentHash {
		return run, errors.New("analysis cache key and asset hash are required")
	}
	if run.Status != "queued" && run.Status != "running" && run.Status != "completed" && run.Status != "failed" && run.Status != "cancelled" {
		return run, errors.New("invalid analysis status")
	}
	run.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(run)
	if err != nil {
		return run, err
	}
	_, err = s.db.Exec(`INSERT INTO analysis_runs(project_id,asset_id,cache_key,body) VALUES(?,?,?,?) ON CONFLICT(project_id,asset_id,cache_key) DO UPDATE SET body=excluded.body`, run.ProjectID, run.AssetID, run.CacheKey, b)
	return run, err
}

func (s *Store) AnalysisRun(project, asset, cacheKey string) (domain.AnalysisRun, error) {
	var run domain.AnalysisRun
	err := decode(s.db.QueryRow("SELECT body FROM analysis_runs WHERE project_id=? AND asset_id=? AND cache_key=?", project, asset, cacheKey), &run)
	return run, err
}

func (s *Store) AnalysisRuns(project, asset string) ([]domain.AnalysisRun, error) {
	rows, err := s.db.Query("SELECT body FROM analysis_runs WHERE project_id=? AND asset_id=? ORDER BY cache_key", project, asset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AnalysisRun{}
	for rows.Next() {
		var b []byte
		var run domain.AnalysisRun
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &run); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) CreateDraft(p domain.DraftPlan) (domain.DraftPlan, error) {
	asset, err := s.Asset(p.ProjectID, p.AssetID)
	if err != nil {
		return p, err
	}
	if err = p.Validate(asset); err != nil {
		return p, err
	}
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	b, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	_, err = s.db.Exec("INSERT INTO draft_plans(id,project_id,asset_id,version,body) VALUES(?,?,?,?,?)", p.ID, p.ProjectID, p.AssetID, p.Version, b)
	return p, err
}

func (s *Store) Draft(id string) (p domain.DraftPlan, err error) {
	err = decode(s.db.QueryRow("SELECT body FROM draft_plans WHERE id=?", id), &p)
	return
}

func (s *Store) UpdateDraft(p domain.DraftPlan, baseVersion int) (domain.DraftPlan, error) {
	if baseVersion < 1 || p.Version != baseVersion+1 {
		return p, ErrConflict
	}
	asset, err := s.Asset(p.ProjectID, p.AssetID)
	if err != nil {
		return p, err
	}
	if err = p.Validate(asset); err != nil {
		return p, err
	}
	p.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	r, err := s.db.Exec("UPDATE draft_plans SET version=?,body=? WHERE id=? AND version=?", p.Version, b, p.ID, baseVersion)
	if err != nil {
		return p, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return p, err
	}
	if n != 1 {
		return p, ErrConflict
	}
	return p, nil
}
func (s *Store) CreateTimeline(t domain.TimelineRevision) error {
	if t.Revision != 1 {
		return errors.New("new timeline must start at revision 1")
	}
	assets, err := s.Assets(t.ProjectID)
	if err != nil {
		return err
	}
	if err = t.Validate(assets); err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO timelines VALUES(?,?,?)", t.ID, t.ProjectID, 1); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO revisions VALUES(?,?,?)", t.ID, 1, b); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmDraft atomically freezes the reviewed version and creates its timeline.
func (s *Store) ConfirmDraft(p domain.DraftPlan, t domain.TimelineRevision) error {
	assets, err := s.Assets(p.ProjectID)
	if err != nil {
		return err
	}
	if err = t.Validate(assets); err != nil {
		return err
	}
	base := p.Version
	p.Version++
	p.Status = "confirmed"
	p.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	timeline, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE draft_plans SET version=?,body=? WHERE id=? AND version=? AND json_extract(body,'$.status')='draft'", p.Version, body, p.ID, base)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	if _, err = tx.Exec("INSERT INTO timelines VALUES(?,?,?)", t.ID, t.ProjectID, 1); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO revisions VALUES(?,?,?)", t.ID, 1, timeline); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Current(id string) (t domain.TimelineRevision, err error) {
	err = decode(s.db.QueryRow("SELECT r.body FROM revisions r JOIN timelines t ON r.timeline_id=t.id AND r.revision=t.revision WHERE t.id=?", id), &t)
	return
}
func (s *Store) Revision(id string, revision int) (t domain.TimelineRevision, err error) {
	err = decode(s.db.QueryRow("SELECT body FROM revisions WHERE timeline_id=? AND revision=?", id, revision), &t)
	return
}
func (s *Store) History(id string) ([]domain.TimelineRevision, error) {
	rows, err := s.db.Query("SELECT body FROM revisions WHERE timeline_id=? ORDER BY revision", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TimelineRevision{}
	for rows.Next() {
		var b []byte
		var t domain.TimelineRevision
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SessionSummary describes one stored conversation for a sidebar listing. The
// transcript itself stays in the body so listing never pays for full decode.
type SessionSummary struct {
	ID        string    `json:"id"`
	Preview   string    `json:"preview,omitempty"`
	Messages  int       `json:"messages"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PutSession upserts a conversation transcript. Preview and Messages are stored
// alongside the body so the sidebar can be listed without decoding every
// transcript.
func (s *Store) PutSession(id string, body []byte, preview string, messages int) error {
	if id == "" || len(body) == 0 {
		return errors.New("session requires id and body")
	}
	_, err := s.db.Exec(`INSERT INTO sessions(id,preview,messages,updated_at,body) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET preview=excluded.preview, messages=excluded.messages, updated_at=excluded.updated_at, body=excluded.body`,
		id, preview, messages, time.Now().UTC().UnixNano(), body)
	return err
}

func (s *Store) Session(id string) ([]byte, error) {
	var body []byte
	if err := s.db.QueryRow("SELECT body FROM sessions WHERE id=?", id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return body, nil
}

func (s *Store) Sessions(limit int) ([]SessionSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query("SELECT id,preview,messages,updated_at FROM sessions ORDER BY updated_at DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionSummary{}
	for rows.Next() {
		var item SessionSummary
		var updated int64
		if err = rows.Scan(&item.ID, &item.Preview, &item.Messages, &updated); err != nil {
			return nil, err
		}
		item.UpdatedAt = time.Unix(0, updated).UTC()
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id=?", id)
	return err
}

func (s *Store) PutAnalysisTask(t domain.AnalysisTask) (domain.AnalysisTask, error) {
	if t.ID == "" || t.ProjectID == "" || t.AssetID == "" {
		return t, errors.New("analysis task requires id, project and asset")
	}
	switch t.Status {
	case domain.TaskQueued, domain.TaskRunning, domain.TaskCompleted, domain.TaskFailed, domain.TaskCancelled, domain.TaskInterrupted:
	default:
		return t, errors.New("invalid analysis task status")
	}
	t.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(t)
	if err != nil {
		return t, err
	}
	_, err = s.db.Exec(`INSERT INTO ui_tasks(id,project_id,asset_id,status,body) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET status=excluded.status, body=excluded.body`,
		t.ID, t.ProjectID, t.AssetID, t.Status, b)
	return t, err
}

func (s *Store) AnalysisTask(id string) (t domain.AnalysisTask, err error) {
	err = decode(s.db.QueryRow("SELECT body FROM ui_tasks WHERE id=?", id), &t)
	return
}

func (s *Store) AnalysisTasks(limit int) ([]domain.AnalysisTask, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query("SELECT body FROM ui_tasks ORDER BY rowid DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AnalysisTask{}
	for rows.Next() {
		var b []byte
		var t domain.AnalysisTask
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAnalysisTask(id string) error {
	_, err := s.db.Exec("DELETE FROM ui_tasks WHERE id=?", id)
	return err
}

// ReconcileAnalysisTasks runs once at startup. No worker goroutine survives a
// restart, so any task still claiming to be queued or running belongs to a dead
// process; marking it interrupted tells the user the truth instead of leaving a
// job that appears to progress forever.
func (s *Store) ReconcileAnalysisTasks() (int, error) {
	rows, err := s.db.Query("SELECT body FROM ui_tasks WHERE status IN (?,?)", domain.TaskQueued, domain.TaskRunning)
	if err != nil {
		return 0, err
	}
	stale := []domain.AnalysisTask{}
	for rows.Next() {
		var b []byte
		var t domain.AnalysisTask
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return 0, err
		}
		if err = json.Unmarshal(b, &t); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, t)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for i := range stale {
		stale[i].Status = domain.TaskInterrupted
		stale[i].Error = "服务在任务完成前重启，任务已中断"
		if _, err = s.PutAnalysisTask(stale[i]); err != nil {
			return i, err
		}
	}
	return len(stale), nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func operationResult(ctx context.Context, q queryer, op domain.EditOperation) (t domain.TimelineRevision, found bool, err error) {
	var payload, body []byte
	err = q.QueryRowContext(ctx, `SELECT o.payload,r.body FROM operations o JOIN revisions r ON r.timeline_id=o.timeline_id AND r.revision=o.revision WHERE o.timeline_id=? AND o.id=?`, op.TimelineID, op.ID).Scan(&payload, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	want, _ := json.Marshal(op)
	if string(payload) != string(want) {
		return t, false, ErrOperationReuse
	}
	err = json.Unmarshal(body, &t)
	return t, true, err
}
func (s *Store) OperationResult(ctx context.Context, op domain.EditOperation) (domain.TimelineRevision, bool, error) {
	return operationResult(ctx, s.db, op)
}

// Save atomically checks the base revision and records BOTH the snapshot and operation.
func (s *Store) Save(ctx context.Context, t domain.TimelineRevision, op domain.EditOperation) (domain.TimelineRevision, error) {
	assets, err := s.Assets(t.ProjectID)
	if err != nil {
		return t, err
	}
	if err = t.Validate(assets); err != nil {
		return t, err
	}
	if t.ID != op.TimelineID || t.Revision != op.BaseRevision+1 {
		return t, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	if prev, ok, e := operationResult(ctx, tx, op); e != nil || ok {
		return prev, e
	}
	r, err := tx.ExecContext(ctx, "UPDATE timelines SET revision=? WHERE id=? AND project_id=? AND revision=?", t.Revision, t.ID, t.ProjectID, op.BaseRevision)
	if err != nil {
		return t, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return t, err
	}
	if n != 1 {
		return t, ErrConflict
	}
	b, _ := json.Marshal(t)
	payload, _ := json.Marshal(op)
	if _, err = tx.ExecContext(ctx, "INSERT INTO revisions VALUES(?,?,?)", t.ID, t.Revision, b); err != nil {
		return t, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,?,?)", t.ID, op.ID, payload, t.Revision); err != nil {
		return t, err
	}
	return t, tx.Commit()
}

func (s *Store) CreateJob(j domain.RenderJob) error {
	if j.ID == "" || (j.Status != "queued" && j.Status != "running") || (j.Kind != "export" && j.Kind != "preview") {
		return errors.New("invalid render job")
	}
	j.UpdatedAt = time.Now().UTC()
	b, _ := json.Marshal(j)
	_, err := s.db.Exec("INSERT INTO jobs VALUES(?,?,?,?,?)", j.ID, j.TimelineID, j.Revision, j.Status, b)
	return err
}

func (s *Store) StartJob(id string) (domain.RenderJob, error) {
	j, err := s.Job(id)
	if err != nil {
		return j, err
	}
	if j.Status != "queued" {
		return j, fmt.Errorf("job state conflict: %s", id)
	}
	j.Status, j.Progress, j.UpdatedAt = "running", 1, time.Now().UTC()
	b, _ := json.Marshal(j)
	r, err := s.db.Exec("UPDATE jobs SET status=?,body=? WHERE id=? AND status='queued'", j.Status, b, id)
	if err != nil {
		return j, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return j, fmt.Errorf("job state conflict: %s", id)
	}
	return j, nil
}

func (s *Store) CancelQueuedJob(id string) (domain.RenderJob, error) {
	j, err := s.Job(id)
	if err != nil {
		return j, err
	}
	if j.Status != "queued" {
		return j, fmt.Errorf("job state conflict: %s", id)
	}
	j.Status, j.Error, j.UpdatedAt = "cancelled", "cancelled by caller", time.Now().UTC()
	b, _ := json.Marshal(j)
	r, err := s.db.Exec("UPDATE jobs SET status=?,body=? WHERE id=? AND status='queued'", j.Status, b, id)
	if err != nil {
		return j, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return j, fmt.Errorf("job state conflict: %s", id)
	}
	return j, nil
}
func (s *Store) FinishJob(j domain.RenderJob) error {
	if j.Status != "completed" && j.Status != "failed" && j.Status != "cancelled" {
		return errors.New("invalid terminal job status")
	}
	if j.Status == "completed" && (j.Validation == nil || !j.Validation.ProbePassed || !j.Validation.DecodePassed) {
		return errors.New("job requires verified output")
	}
	j.UpdatedAt = time.Now().UTC()
	b, _ := json.Marshal(j)
	r, err := s.db.Exec("UPDATE jobs SET status=?,body=? WHERE id=? AND status='running'", j.Status, b, j.ID)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return fmt.Errorf("job state conflict: %s", j.ID)
	}
	return nil
}
func (s *Store) Job(id string) (j domain.RenderJob, err error) {
	err = decode(s.db.QueryRow("SELECT body FROM jobs WHERE id=?", id), &j)
	return
}

func (s *Store) Jobs() ([]domain.RenderJob, error) {
	rows, err := s.db.Query("SELECT body FROM jobs ORDER BY rowid DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []domain.RenderJob{}
	for rows.Next() {
		var b []byte
		var j domain.RenderJob
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
