package agent

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/zylar06/video-agent/internal/analysis/provider"
	"github.com/zylar06/video-agent/internal/app"
)

// Session is one ongoing conversation with the editing agent. It owns the
// transcript so a page reload can resume where the user left off, and it
// serializes turns so two messages cannot interleave into one history.
type Session struct {
	ID      string
	History *History

	mu      sync.Mutex
	running bool
}

// TryBegin claims the session for one turn. It fails if a turn is already
// running, which keeps concurrent writers out of the shared transcript.
func (s *Session) TryBegin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("这一轮还在进行中，请等它结束再发下一条")
	}
	s.running = true
	return nil
}

func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
func (s *Session) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
}

// Runtime owns the live sessions and builds a configured runner per turn.
type Runtime struct {
	App      *app.App
	Model    provider.OpenAIText
	System   string
	MaxSteps int

	mu       sync.Mutex
	sessions map[string]*Session
	order    []string
}

// SystemPrompt states the operating contract: the model is a video editing
// assistant that must ground every claim in tool output rather than inventing
// timestamps, which is the single most damaging failure mode for this product.
const SystemPrompt = `你是 Video Agent，一个本地长视频理解与智能剪辑助手。你可以调用工具来完成工作。

工作要求：
1. 先弄清现状再动手：不知道项目或素材 id 时，先调用 project_list / assets_list 查询，不要猜 id。
2. 按内容找片段必须基于证据：需要先对素材调用 analyze，再用 search 检索，最后用 timeline_create 或 proposal_create 组装成片。
3. 时间戳只能来自工具返回的证据或素材时长，绝对不要自己编造时间或素材内容。检索没有命中时，如实说明素材里没有这种内容，不要拿别的内容凑数。
4. 编辑已有时间线前先 timeline_get 拿到当前 revision；edit_apply 的 base_revision 必须等于当前版本。
5. 分清代价：查询、分析、创建草稿时间线可以直接做；但 render_submit 会真实消耗时间并写出文件，必须先把你打算剪成什么样讲清楚并等用户同意，不要自作主张就开始渲染。渲染提交后用 jobs_get 报进度。
6. 一次只走必要的步骤。工具结果被截断时改用更精确的参数再查，不要用同样的参数重复调用。
7. 面向用户回答时用简洁中文，说明你做了什么、依据是什么。工具失败时读错误信息并换一种做法，不要把原始报错直接抛给用户。`

func NewRuntime(a *app.App, model provider.OpenAIText) *Runtime {
	return &Runtime{App: a, Model: model, System: SystemPrompt, MaxSteps: DefaultMaxSteps, sessions: map[string]*Session{}}
}

// Session returns the live session, restoring it from storage on first use so a
// restarted service resumes the conversation instead of silently starting over.
func (r *Runtime) Session(id string) *Session {
	r.mu.Lock()
	if s, ok := r.sessions[id]; ok {
		r.mu.Unlock()
		return s
	}
	s := &Session{ID: id, History: NewHistory()}
	r.sessions[id] = s
	r.mu.Unlock()

	if r.App != nil && r.App.Store != nil {
		_ = r.App.Store.CreateSession(id, "")
		if stored, err := r.App.Store.SessionMessages(id); err == nil && len(stored) > 0 {
			state, stateErr := r.App.Store.SessionState(id)
			if stateErr != nil {
				// A corrupt state blob must not also lose what the user sees.
				state = nil
			}
			s.History.SetRestored(state, stored)
		}
	}
	s.History.AttachSink(sessionSink{runtime: r, id: id})
	return s
}

// sessionSink adapts the store to the loop's persistence hook.
type sessionSink struct {
	runtime *Runtime
	id      string
}

func (s sessionSink) AppendMessage(view View) error {
	if s.runtime.App == nil || s.runtime.App.Store == nil {
		return nil
	}
	return s.runtime.App.Store.AppendSessionMessage(s.id, view)
}

func (s sessionSink) SaveState(messages []Message) error {
	if s.runtime.App == nil || s.runtime.App.Store == nil {
		return nil
	}
	return s.runtime.App.Store.SaveSessionState(s.id, messages)
}

// Summary describes one conversation for a sidebar.
type Summary struct {
	ID        string    `json:"id"`
	Running   bool      `json:"running"`
	Messages  int       `json:"messages"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Sessions lists conversations, most recently updated first. It reads from the
// store when available so conversations survive a restart, and merges live
// running state for sessions currently in memory.
func (r *Runtime) Sessions() []Summary {
	byID := map[string]Summary{}
	if r.App != nil && r.App.Store != nil {
		if stored, err := r.App.Store.Sessions(); err == nil {
			for _, info := range stored {
				byID[info.ID] = Summary{ID: info.ID, Messages: info.Messages, Title: info.Title, UpdatedAt: info.UpdatedAt}
			}
		}
	}
	r.mu.Lock()
	for id, s := range r.sessions {
		summary := byID[id]
		summary.ID = id
		summary.Running = s.Running()
		if n := s.History.Len(); n > summary.Messages {
			summary.Messages = n
		}
		byID[id] = summary
	}
	r.mu.Unlock()

	out := make([]Summary, 0, len(byID))
	for _, summary := range byID {
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// Run executes one turn of a session, streaming events to emit.
func (r *Runtime) Run(ctx context.Context, sessionID, message string, emit Emit) error {
	if r.App == nil {
		return errors.New("agent runtime has no application")
	}
	session := r.Session(sessionID)
	if err := session.TryBegin(); err != nil {
		return err
	}
	defer session.End()

	runner := Runner{
		Model:    r.Model,
		Tools:    NewService(r.App),
		System:   r.System,
		MaxSteps: r.MaxSteps,
		Consent:  session.History.UserConsented,
	}
	err := runner.Turn(ctx, session.History, message, emit)
	// Persist the model-facing history even when the turn failed partway: the
	// work already done is exactly what a resumed conversation needs.
	_ = sessionSink{runtime: r, id: sessionID}.SaveState(session.History.MessagesCopy())
	return err
}

// Transcript returns the renderable history of a session.
func (r *Runtime) Transcript(sessionID string) []View {
	return r.Session(sessionID).History.View()
}

// DeleteSession drops a conversation from memory and storage.
func (r *Runtime) DeleteSession(id string) error {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
	if r.App == nil || r.App.Store == nil {
		return nil
	}
	return r.App.Store.DeleteSession(id)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// SortedSessionIDs is a helper for deterministic tests.
func (r *Runtime) SortedSessionIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}
