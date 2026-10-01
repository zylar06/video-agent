package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zylar06/video-agent/internal/analysis/asr"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/media"
)

type Config struct {
	BaseURL, Model, APIKey string
	HTTPClient             *http.Client
}

func ConfigFromEnv(prefix string) Config {
	return Config{BaseURL: os.Getenv(prefix + "_BASE_URL"), Model: os.Getenv(prefix + "_MODEL"), APIKey: os.Getenv(prefix + "_API_KEY"), HTTPClient: http.DefaultClient}
}

// ConfigFromEnvAliases lets Video Agent reuse compatible GoClip environment
// groups without sharing GoClip's encrypted SQLite secrets database.
func ConfigFromEnvAliases(prefixes ...string) Config {
	for _, prefix := range prefixes {
		config := ConfigFromEnv(prefix)
		if config.BaseURL != "" || config.Model != "" || config.APIKey != "" {
			return config
		}
	}
	return Config{HTTPClient: http.DefaultClient}
}

type OpenAITranscriber struct{ Config Config }

// QwenASR adapts Qwen3 ASR's OpenAI-compatible chat endpoint.  The service
// accepts audio in five-minute windows; extracting compact audio here keeps
// video bytes and browser uploads out of the model request.
type QwenASR struct {
	Config Config
	Tools  media.Tools
}

// QwenAudioASR uses Bailian's native Qwen-Audio endpoint because the
// OpenAI-compatible Qwen3 ASR response does not contain timestamps.
type QwenAudioASR struct {
	Config Config
	Tools  media.Tools
}

type OpenAIText struct{ Config Config }

func (p OpenAIText) Complete(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if p.Config.BaseURL == "" || p.Config.Model == "" || p.Config.APIKey == "" {
		return "", errors.New("model provider unavailable: text provider is not configured")
	}
	payload := map[string]any{"model": p.Config.Model, "temperature": 0, "response_format": map[string]string{"type": "json_object"}, "messages": []any{map[string]string{"role": "system", "content": "你是本地视频剪辑助手。严格按用户请求输出 JSON，不要编造时间戳或证据 ID。"}, map[string]string{"role": "user", "content": prompt}}}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(p.Config.BaseURL, "chat/completions"), bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client(p.Config).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("text provider returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", errors.New("text provider returned empty response")
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content), nil
}

func (p OpenAITranscriber) Transcribe(ctx context.Context, asset domain.MediaAsset) ([]asr.Cue, error) {
	if p.Config.BaseURL == "" || p.Config.Model == "" || p.Config.APIKey == "" {
		return nil, errors.New("model provider unavailable: ASR provider is not configured")
	}
	f, err := os.Open(asset.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", p.Config.Model); err != nil {
		return nil, err
	}
	if err := form.WriteField("response_format", "verbose_json"); err != nil {
		return nil, err
	}
	if err := form.WriteField("timestamp_granularities[]", "segment"); err != nil {
		return nil, err
	}
	part, err := form.CreateFormFile("file", filepath.Base(asset.Path))
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(part, f); err != nil {
		return nil, err
	}
	if err = form.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(p.Config.BaseURL, "audio/transcriptions"), &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := client(p.Config).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ASR provider returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Text     string `json:"text"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("invalid ASR response: %w", err)
	}
	if len(decoded.Segments) > 0 {
		out := make([]asr.Cue, 0, len(decoded.Segments))
		for i, seg := range decoded.Segments {
			if strings.TrimSpace(seg.Text) != "" && seg.End > seg.Start {
				out = append(out, asr.Cue{Index: i + 1, StartUS: int64(seg.Start*1e6 + 0.5), EndUS: int64(seg.End*1e6 + 0.5), Text: strings.TrimSpace(seg.Text)})
			}
		}
		return out, nil
	}
	if strings.TrimSpace(decoded.Text) == "" {
		return nil, errors.New("ASR provider returned empty transcript")
	}
	return []asr.Cue{{Index: 1, StartUS: 0, EndUS: asset.DurationUS, Text: strings.TrimSpace(decoded.Text)}}, nil
}

func (p QwenASR) Transcribe(ctx context.Context, asset domain.MediaAsset) ([]asr.Cue, error) {
	if p.Config.BaseURL == "" || p.Config.Model == "" || p.Config.APIKey == "" {
		return nil, errors.New("model provider unavailable: ASR provider is not configured")
	}
	if !asset.HasAudio {
		return nil, errors.New("asset has no audio track")
	}
	tools := p.Tools
	if tools.FFmpeg == "" {
		tools = media.Default()
	}
	const windowUS int64 = 240_000_000
	var cues []asr.Cue
	for start := int64(0); start < asset.DurationUS; start += windowUS {
		end := min(start+windowUS, asset.DurationUS)
		file, err := os.CreateTemp("", "video-agent-asr-*.mp3")
		if err != nil {
			return nil, err
		}
		path := file.Name()
		_ = file.Close()
		_, err = media.Run(ctx, tools.FFmpeg, "-nostdin", "-v", "error", "-ss", fmt.Sprintf("%.3f", float64(start)/1e6), "-t", fmt.Sprintf("%.3f", float64(end-start)/1e6), "-i", asset.Path, "-vn", "-ac", "1", "-ar", "16000", "-b:a", "32k", "-y", path)
		if err != nil {
			_ = os.Remove(path)
			return nil, err
		}
		data, readErr := os.ReadFile(path)
		_ = os.Remove(path)
		if readErr != nil {
			return nil, readErr
		}
		if len(data) == 0 || len(data) > 10<<20 {
			return nil, errors.New("prepared ASR audio is empty or exceeds the provider limit")
		}
		payload := map[string]any{
			"model":       p.Config.Model,
			"messages":    []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio", "input_audio": map[string]string{"data": "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(data)}}}}},
			"asr_options": map[string]any{"enable_itn": true},
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(p.Config.BaseURL, "chat/completions"), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client(p.Config).Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("Qwen ASR returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var decoded struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
			return nil, errors.New("Qwen ASR returned empty transcript")
		}
		cues = append(cues, asr.Cue{Index: len(cues) + 1, StartUS: start, EndUS: end, Text: strings.TrimSpace(decoded.Choices[0].Message.Content)})
	}
	return cues, nil
}

func (p QwenAudioASR) Transcribe(ctx context.Context, asset domain.MediaAsset) ([]asr.Cue, error) {
	if p.Config.BaseURL == "" || p.Config.Model == "" || p.Config.APIKey == "" {
		return nil, errors.New("model provider unavailable: ASR provider is not configured")
	}
	if !asset.HasAudio {
		return nil, errors.New("asset has no audio track")
	}
	tools := p.Tools
	if tools.FFmpeg == "" {
		tools = media.Default()
	}
	const windowUS int64 = 240_000_000
	var cues []asr.Cue
	for start := int64(0); start < asset.DurationUS; start += windowUS {
		end := min(start+windowUS, asset.DurationUS)
		file, err := os.CreateTemp("", "video-agent-asr-*.mp3")
		if err != nil {
			return nil, err
		}
		path := file.Name()
		_ = file.Close()
		_, err = media.Run(ctx, tools.FFmpeg, "-nostdin", "-v", "error", "-ss", fmt.Sprintf("%.3f", float64(start)/1e6), "-t", fmt.Sprintf("%.3f", float64(end-start)/1e6), "-i", asset.Path, "-vn", "-ac", "1", "-ar", "16000", "-b:a", "32k", "-y", path)
		if err != nil {
			_ = os.Remove(path)
			return nil, err
		}
		data, readErr := os.ReadFile(path)
		_ = os.Remove(path)
		if readErr != nil {
			return nil, readErr
		}
		if len(data) == 0 || len(data) > 10<<20 {
			return nil, errors.New("prepared ASR audio is empty or exceeds the provider limit")
		}
		window, err := p.transcribeWindow(ctx, data)
		if err != nil {
			return nil, err
		}
		window = normalizeCumulativeQwenCues(window)
		for _, cue := range window {
			cue.Index = len(cues) + 1
			cue.StartUS += start
			cue.EndUS += start
			if cue.EndUS > asset.DurationUS {
				cue.EndUS = asset.DurationUS
			}
			if cue.EndUS > cue.StartUS {
				cues = append(cues, cue)
			}
		}
	}
	if len(cues) == 0 {
		return nil, errors.New("Qwen Audio ASR returned no timestamped sentences")
	}
	return cues, nil
}

// normalizeCumulativeQwenCues handles Qwen Audio's incremental SSE mode.
// Some model versions emit a growing transcript on every completed event,
// while retaining the first event's start time. Retaining those events would
// create overlapping, increasingly long evidence. Convert a strict text
// prefix into the new timed delta instead.
func normalizeCumulativeQwenCues(cues []asr.Cue) []asr.Cue {
	out := make([]asr.Cue, 0, len(cues))
	var previous asr.Cue
	for i, cue := range cues {
		cue.Text = strings.TrimSpace(cue.Text)
		if i > 0 && cue.StartUS <= previous.StartUS+1_000_000 && cue.EndUS > previous.EndUS && strings.HasPrefix(cue.Text, previous.Text) {
			cue.Text = strings.TrimSpace(strings.TrimPrefix(cue.Text, previous.Text))
			cue.StartUS = previous.EndUS
		}
		if cue.EndUS > cue.StartUS && cue.Text != "" {
			cue.Index = len(out) + 1
			out = append(out, cue)
		}
		previous = cues[i]
		previous.Text = strings.TrimSpace(previous.Text)
	}
	return out
}

func (p QwenAudioASR) transcribeWindow(ctx context.Context, data []byte) ([]asr.Cue, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	payload := map[string]any{
		"model":      p.Config.Model,
		"input":      map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio", "input_audio": map[string]string{"data": "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(data)}}}}}},
		"parameters": map[string]any{"format": "mp3", "sample_rate": "16000"},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwenAudioEndpoint(p.Config.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")
	resp, err := client(p.Config).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return nil, fmt.Errorf("Qwen Audio ASR returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		cue, err := parseQwenAudioJSON(raw)
		if err != nil {
			return nil, err
		}
		return []asr.Cue{cue}, nil
	}
	return parseQwenAudioSSE(bytes.NewReader(raw))
}

func parseQwenAudioJSON(raw []byte) (asr.Cue, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(raw, &event); err != nil {
		return asr.Cue{}, fmt.Errorf("invalid Qwen Audio ASR response: %w", err)
	}
	var output map[string]json.RawMessage
	if err := json.Unmarshal(event["output"], &output); err != nil {
		return asr.Cue{}, errors.New("Qwen Audio ASR response omitted output")
	}
	var sentence map[string]any
	if err := json.Unmarshal(output["sentence"], &sentence); err != nil {
		return asr.Cue{}, errors.New("Qwen Audio ASR response omitted sentence")
	}
	id, idOK := sentence["sentence_id"].(float64)
	start, startOK := sentence["begin_time"].(float64)
	end, endOK := sentence["end_time"].(float64)
	done, doneOK := sentence["sentence_end"].(bool)
	text, textOK := sentence["text"].(string)
	if !idOK || !startOK || !endOK || !doneOK || !textOK || !done || id < 1 || end <= start || strings.TrimSpace(text) == "" {
		return asr.Cue{}, errors.New("Qwen Audio ASR response has no completed sentence")
	}
	return asr.Cue{Index: int(id), StartUS: int64(start * 1000), EndUS: int64(end * 1000), Text: strings.TrimSpace(text)}, nil
}

func parseQwenAudioSSE(r io.Reader) ([]asr.Cue, error) {
	type sentence struct {
		ID    int    `json:"sentence_id"`
		Start int64  `json:"begin_time"`
		End   int64  `json:"end_time"`
		Done  bool   `json:"sentence_end"`
		Text  string `json:"text"`
	}
	seen := map[int]bool{}
	var cues []asr.Cue
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Output struct {
				Sentence sentence `json:"sentence"`
			} `json:"output"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			return nil, fmt.Errorf("invalid Qwen Audio ASR event: %w", err)
		}
		s := event.Output.Sentence
		if !s.Done || s.ID < 1 || seen[s.ID] || s.End <= s.Start || strings.TrimSpace(s.Text) == "" {
			continue
		}
		seen[s.ID] = true
		cues = append(cues, asr.Cue{Index: len(cues) + 1, StartUS: s.Start * 1000, EndUS: s.End * 1000, Text: strings.TrimSpace(s.Text)})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(cues) == 0 {
		return nil, errors.New("Qwen Audio ASR returned no completed sentences")
	}
	return cues, nil
}

type OpenAIVision struct{ Config Config }

func (p OpenAIVision) Describe(ctx context.Context, evidence []domain.Evidence) (map[string]string, error) {
	if p.Config.BaseURL == "" || p.Config.Model == "" || p.Config.APIKey == "" {
		return nil, errors.New("model provider unavailable: vision provider is not configured")
	}
	return p.describeBatches(ctx, evidence)
}

const visionBatchSize = 8

// Keep one in-flight batch by default: some hosted VL workspaces queue
// concurrent image requests instead of rejecting them with a rate-limit code.
const visionWorkers = 1

func (p OpenAIVision) describeBatches(ctx context.Context, evidence []domain.Evidence) (map[string]string, error) {
	if len(evidence) == 0 {
		return map[string]string{}, nil
	}
	type batchResult struct {
		summaries map[string]string
		err       error
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerCount := min(visionWorkers, (len(evidence)+visionBatchSize-1)/visionBatchSize)
	jobs := make(chan []domain.Evidence)
	results := make(chan batchResult, workerCount)
	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for batch := range jobs {
				summaries, err := p.describeBatch(workCtx, batch)
				if err != nil && workCtx.Err() == nil {
					summaries, err = p.describeBatch(workCtx, batch)
				}
				select {
				case results <- batchResult{summaries: summaries, err: err}:
				case <-workCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for start := 0; start < len(evidence); start += visionBatchSize {
			end := min(start+visionBatchSize, len(evidence))
			select {
			case jobs <- evidence[start:end]:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	out := map[string]string{}
	for result := range results {
		if result.err != nil {
			return nil, result.err
		}
		for id, summary := range result.summaries {
			out[id] = summary
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (p OpenAIVision) describeBatch(ctx context.Context, evidence []domain.Evidence) (map[string]string, error) {
	out := map[string]string{}
	for start := 0; start < len(evidence); start += visionBatchSize {
		end := min(start+visionBatchSize, len(evidence))
		content := []any{map[string]any{"type": "text", "text": "按时间顺序观察这些连续视频帧。只返回 JSON 对象，包含 frames 数组；数组每项必须有 id 和 summary，id 必须等于输入帧 ID，summary 是一句可核对的中文画面描述。不要猜测画外信息。"}}
		want := map[string]bool{}
		wireIDs := map[string]string{}
		for index, frame := range evidence[start:end] {
			if len(frame.FrameRefs) == 0 {
				return nil, errors.New("visual evidence is missing its frame")
			}
			data, err := os.ReadFile(frame.FrameRefs[0])
			if err != nil {
				return nil, err
			}
			want[frame.ID] = true
			wireID := fmt.Sprintf("f%d", index)
			wireIDs[wireID] = frame.ID
			content = append(content, map[string]any{"type": "text", "text": "下一张图片的字符串 id：" + wireID})
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)}})
		}
		payload := map[string]any{"model": p.Config.Model, "temperature": 0, "response_format": map[string]string{"type": "json_object"}, "messages": []any{map[string]any{"role": "user", "content": content}}}
		body, _ := json.Marshal(payload)
		requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint(p.Config.BaseURL, "chat/completions"), bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client(p.Config).Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		cancel()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("vision provider returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, errors.New("vision provider returned invalid JSON")
		}
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(response["choices"], &choices); err != nil || len(choices) == 0 {
			return nil, errors.New("vision provider returned no choices")
		}
		var message map[string]string
		if err := json.Unmarshal(choices[0]["message"], &message); err != nil || strings.TrimSpace(message["content"]) == "" {
			return nil, errors.New("vision provider returned no content")
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal([]byte(message["content"]), &decoded); err != nil {
			return nil, errors.New("visual batch is not structured JSON; no frame descriptions accepted")
		}
		var frames []map[string]any
		if err := json.Unmarshal(decoded["frames"], &frames); err != nil {
			return nil, errors.New("vision provider did not return a frames list")
		}
		found := map[string]bool{}
		for _, frame := range frames {
			id, _ := frame["id"].(string)
			if actual, ok := wireIDs[id]; ok {
				id = actual
			}
			if index, ok := frame["id"].(float64); ok && index >= 0 && index == math.Trunc(index) && int(index) < end-start {
				id = evidence[start+int(index)].ID
			}
			summary, _ := frame["summary"].(string)
			if want[id] && strings.TrimSpace(summary) != "" {
				out[id] = strings.TrimSpace(summary)
				found[id] = true
			}
		}
		for _, frame := range evidence[start:end] {
			if found[frame.ID] {
				continue
			}
			return nil, fmt.Errorf("visual description missing for %s", frame.ID)
		}
	}
	return out, nil
}

func endpoint(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/" + suffix
	}
	return base + "/v1/" + suffix
}

func qwenAudioEndpoint(base string) string {
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/compatible-mode/v1")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/api/v1/services/aigc/multimodal-generation/generation"
}
func client(c Config) *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}
