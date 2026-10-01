package chat

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zylar06/video-agent/internal/analysis"
	"github.com/zylar06/video-agent/internal/analysis/provider"
	"github.com/zylar06/video-agent/internal/catalog"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/store"
)

type Request struct {
	ProjectID    string `json:"project_id"`
	AssetID      string `json:"asset_id"`
	Message      string `json:"message"`
	SubtitlePath string `json:"subtitle_path,omitempty"`
	Visual       bool   `json:"visual,omitempty"`
}

type Intent struct {
	Goal       string `json:"goal"`
	Query      string `json:"query,omitempty"`
	DurationUS int64  `json:"duration_us,omitempty"`
	NeedsEdit  bool   `json:"needs_edit"`
}

type Result struct {
	Reply    string                 `json:"reply"`
	Intent   Intent                 `json:"intent"`
	Evidence []catalog.SearchResult `json:"evidence,omitempty"`
}

type Service struct {
	Store    *store.Store
	Text     provider.OpenAIText
	Analyzer *analysis.Service
}

func (s Service) Handle(ctx context.Context, req Request) (Result, error) {
	intent := classify(req.Message)
	if s.Analyzer != nil && (req.SubtitlePath != "" || req.Visual) {
		if _, err := s.Analyzer.Analyze(ctx, analysis.Request{ProjectID: req.ProjectID, AssetID: req.AssetID, SubtitlePath: req.SubtitlePath, Visual: req.Visual}); err != nil {
			return Result{}, err
		}
	}
	if s.Text.Config.BaseURL != "" && s.Text.Config.Model != "" && s.Text.Config.APIKey != "" {
		prompt := "将用户的本地视频剪辑请求映射为 JSON。goal 只能是 select、trim_ends 或 preview；query 是需要在字幕或画面证据中检索的简短关键词，不确定时保留用户的核心词；duration_us 是目标微秒时长，没有明确时长填 0；needs_edit 为 true。只返回 JSON，不要编造时间戳。用户请求：" + req.Message
		if raw, err := s.Text.Complete(ctx, prompt); err == nil {
			var modelIntent Intent
			if json.Unmarshal([]byte(raw), &modelIntent) == nil && valid(modelIntent.Goal) {
				if strings.TrimSpace(modelIntent.Query) == "" {
					modelIntent.Query = intent.Query
				}
				if modelIntent.DurationUS == 0 {
					modelIntent.DurationUS = intent.DurationUS
				}
				modelIntent.NeedsEdit = true
				intent = modelIntent
			}
		}
	}
	if intent.Query != "" {
		items, err := s.Store.SearchEvidence(req.ProjectID, intent.Query, []string{req.AssetID}, 12)
		if err == nil && len(items) > 0 {
			return Result{Reply: reply(intent, len(items)), Intent: intent, Evidence: items}, nil
		}
	}
	// A keyword that matches nothing must not dead-end the request: requests such
	// as "一分钟以上" carry a duration but no searchable term. Fall back to the
	// strongest available clips so the user still gets a reviewable plan.
	if items := s.fallbackCandidates(req.ProjectID, req.AssetID, 12); len(items) > 0 {
		intent.Goal = "best"
		return Result{Reply: reply(intent, len(items)), Intent: intent, Evidence: items}, nil
	}
	return Result{Reply: reply(intent, 0), Intent: intent}, nil
}

// fallbackCandidates ranks evidence for requests that carry no usable keyword.
func (s Service) fallbackCandidates(project, asset string, limit int) []catalog.SearchResult {
	all, err := s.Store.Evidence(project, []string{asset})
	if err != nil {
		return nil
	}
	return rankEvidence(all, limit)
}

// rankEvidence prefers clips the visual analyzer confirmed, then longer coverage.
func rankEvidence(all []domain.Evidence, limit int) []catalog.SearchResult {
	if len(all) == 0 {
		return nil
	}
	ranked := append([]domain.Evidence(nil), all...)
	sort.SliceStable(ranked, func(i, j int) bool {
		vi, vj := ranked[i].VisualSummary != "", ranked[j].VisualSummary != ""
		if vi != vj {
			return vi
		}
		return ranked[i].EndUS-ranked[i].StartUS > ranked[j].EndUS-ranked[j].StartUS
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]catalog.SearchResult, 0, len(ranked))
	for _, e := range ranked {
		out = append(out, catalog.SearchResult{Evidence: e, Reason: "selected from all available evidence"})
	}
	return out
}

var durationRE = regexp.MustCompile(`([0-9]+)\s*(分钟|分|秒)`)

func classify(message string) Intent {
	m := strings.ToLower(strings.TrimSpace(message))
	intent := Intent{Goal: "select", Query: queryWords(m), NeedsEdit: true}
	switch {
	case strings.Contains(m, "高能"):
		intent = Intent{Goal: "highlights", Query: "高能", NeedsEdit: true}
	case strings.Contains(m, "笑"):
		intent = Intent{Goal: "jokes", Query: "笑", NeedsEdit: true}
	case strings.Contains(m, "进球") || strings.Contains(m, "庆祝"):
		intent = Intent{Goal: "sports", Query: "进球 庆祝", NeedsEdit: true}
	case strings.Contains(m, "开场") || strings.Contains(m, "片尾"):
		intent = Intent{Goal: "trim_ends", NeedsEdit: true}
	case strings.Contains(m, "预览"):
		intent = Intent{Goal: "preview", NeedsEdit: true}
	}
	if match := durationRE.FindStringSubmatch(m); len(match) == 3 {
		n, _ := strconv.ParseInt(match[1], 10, 64)
		if match[2] == "秒" {
			intent.DurationUS = n * 1_000_000
		} else {
			intent.DurationUS = n * 60 * 1_000_000
		}
	}
	return intent
}

func valid(goal string) bool {
	return goal == "select" || goal == "highlights" || goal == "jokes" || goal == "sports" || goal == "trim_ends" || goal == "preview"
}

func queryWords(message string) string {
	for _, phrase := range []string{"剪成", "做成", "保留", "删掉", "删除", "所有", "片段", "部分", "视频", "高能集锦"} {
		message = strings.ReplaceAll(message, phrase, " ")
	}
	message = durationRE.ReplaceAllString(message, " ")
	message = strings.NewReplacer("，", " ", "。", " ", ",", " ", ".", " ").Replace(message)
	return strings.Join(strings.Fields(message), " ")
}
func reply(i Intent, count int) string {
	if i.Goal == "trim_ends" {
		return "我可以生成删除开场和片尾的剪辑方案；先确认时间线后再修改。"
	}
	if i.Goal == "preview" {
		return "我可以生成当前剪辑方案的预览；请先确认候选片段。"
	}
	if i.Goal == "best" {
		return "没有找到与「" + i.Query + "」匹配的原话，已改用可信度最高的 " + strconv.Itoa(count) + " 个片段。你可以直接确认，或换一个字幕里出现过的词。"
	}
	if count == 0 {
		return "我暂时没有找到可核对的候选片段，可以换一个关键词或提供字幕。"
	}
	return "我找到 " + strconv.Itoa(count) + " 个有来源的候选片段。请确认后，我再生成剪辑方案。"
}
