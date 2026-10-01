package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

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
5. 按主题成片时走草稿：先 draft_create 得到带来源证据和安全范围的候选，再用 draft_edit 按用户意见逐条微调（“长一点”用 extend_end_us 正数）、锁定或删除，最后 draft_confirm 才生成时间线。确认之前不要碰时间线，也不要用 timeline_create 绕过草稿。
6. 分清代价：查询、分析、创建和修改草稿都可以直接做；但 render_submit 会真实消耗时间并写出文件，必须先把你打算剪成什么样讲清楚并等用户同意，不要自作主张就开始渲染。渲染提交后用 jobs_get 报进度。
7. 一次只走必要的步骤。工具结果被截断时改用更精确的参数再查，不要用同样的参数重复调用。
8. 面向用户回答时用简洁中文，说明你做了什么、依据是什么。工具失败时读错误信息并换一种做法，不要把原始报错直接抛给用户。`

func NewRuntime(a *app.App, model provider.OpenAIText) *Runtime {
	return &Runtime{App: a, Model: model, System: SystemPrompt, MaxSteps: DefaultMaxSteps, sessions: map[string]*Session{}}
}

// Session returns the existing session or creates one, so a client can start
// chatting without a separate handshake.
func (r *Runtime) Session(id string) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		return s
	}
	history := NewHistory()
	if r.App != nil && r.App.Store != nil {
		if body, err := r.App.Store.Session(id); err == nil {
			var snapshot HistorySnapshot
			if json.Unmarshal(body, &snapshot) == nil && snapshot.Version == 1 {
				history.Restore(snapshot)
			}
		}
	}
	s := &Session{ID: id, History: history}
	r.sessions[id] = s
	r.order = append(r.order, id)
	return s
}

// Summary describes one session for a sidebar.
type Summary struct {
	ID       string `json:"id"`
	Running  bool   `json:"running"`
	Messages int    `json:"messages"`
	Preview  string `json:"preview,omitempty"`
}

// Sessions lists known sessions, newest first.
func (r *Runtime) Sessions() []Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Summary, 0, len(r.order))
	seen := map[string]bool{}
	for i := len(r.order) - 1; i >= 0; i-- {
		s := r.sessions[r.order[i]]
		if s == nil {
			continue
		}
		seen[s.ID] = true
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		summary := Summary{ID: s.ID, Running: running, Messages: s.History.Len()}
		view := s.History.View()
		for _, v := range view {
			if v.Role == "user" && v.Text != "" {
				summary.Preview = truncate(v.Text, 40)
				break
			}
		}
		out = append(out, summary)
	}
	if r.App != nil && r.App.Store != nil {
		if stored, err := r.App.Store.Sessions(100); err == nil {
			for _, item := range stored {
				if seen[item.ID] {
					continue
				}
				out = append(out, Summary{ID: item.ID, Messages: item.Messages, Preview: item.Preview})
			}
		}
	}
	return out
}

func (r *Runtime) persist(session *Session) {
	if r.App == nil || r.App.Store == nil || session == nil {
		return
	}
	body, err := json.Marshal(session.History.Snapshot())
	if err != nil {
		return
	}
	preview := ""
	for _, v := range session.History.View() {
		if v.Role == "user" && v.Text != "" {
			preview = truncate(v.Text, 40)
			break
		}
	}
	_ = r.App.Store.PutSession(session.ID, body, preview, session.History.Len())
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
	wrappedEmit := func(event Event) {
		r.persist(session)
		if emit != nil {
			emit(event)
		}
	}
	err := runner.Turn(ctx, session.History, message, wrappedEmit)
	r.persist(session)
	return err
}

// Transcript returns the renderable history of a session.
func (r *Runtime) Transcript(sessionID string) []View {
	return r.Session(sessionID).History.View()
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
