package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The model-facing tool list and the dispatch switch are two halves of one
// contract. If they drift, the model is offered a tool that cannot run, or an
// action becomes unreachable. Both directions are asserted here.
func TestToolSpecsMatchCallableActions(t *testing.T) {
	s := &Service{}
	declared := map[string]bool{}
	for _, spec := range s.ToolSpecs() {
		if spec.Name == "" {
			t.Fatal("tool spec without a name")
		}
		if declared[spec.Name] {
			t.Fatalf("tool %q declared twice", spec.Name)
		}
		declared[spec.Name] = true
		if strings.TrimSpace(spec.Description) == "" {
			t.Fatalf("tool %q has no description for the model", spec.Name)
		}
		if spec.Parameters["type"] != "object" {
			t.Fatalf("tool %q parameters must be a JSON Schema object, got %v", spec.Name, spec.Parameters["type"])
		}
	}

	callable := map[string]bool{}
	for _, name := range s.Names() {
		callable[name] = true
	}

	for name := range callable {
		if !declared[name] {
			t.Errorf("action %q is callable but not offered to the model", name)
		}
	}
	for name := range declared {
		if !callable[name] {
			t.Errorf("tool %q is offered to the model but has no dispatch case", name)
		}
	}
}

func TestToolSpecsAreSortedAndStable(t *testing.T) {
	specs := (&Service{}).ToolSpecs()
	if len(specs) == 0 {
		t.Fatal("no tools declared")
	}
	for i := 1; i < len(specs); i++ {
		if specs[i-1].Name >= specs[i].Name {
			t.Fatalf("tool specs must be sorted by name: %q before %q", specs[i-1].Name, specs[i].Name)
		}
	}
}

func TestModelToolsProjectionHidesExecutionDetail(t *testing.T) {
	tools := (&Service{}).ModelTools()
	if len(tools) != len((&Service{}).ToolSpecs()) {
		t.Fatal("projection dropped tools")
	}
	for _, tool := range tools {
		if tool.Name == "" || tool.Description == "" || tool.Parameters == nil {
			t.Fatalf("incomplete projected tool: %+v", tool)
		}
	}
	// The wire payload must not leak anything beyond the three model-facing keys,
	// including the local read-only classification.
	encoded, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key := range generic {
		switch key {
		case "name", "description", "parameters":
		default:
			t.Fatalf("unexpected model-visible field %q in %s", key, encoded)
		}
	}
}

func TestRunToolRejectsUnknownToolWithoutPanicking(t *testing.T) {
	// A nil App is deliberate: an unknown tool must fail on name resolution
	// before any store access is attempted.
	result, ok := (&Service{}).RunTool(context.Background(), "definitely_not_a_tool", "{}")
	if ok {
		t.Fatal("unknown tool must report failure")
	}
	var envelope Envelope
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		t.Fatalf("tool result must be valid JSON the model can read: %v (%s)", err, result)
	}
	if envelope.OK {
		t.Fatal("envelope should not claim success")
	}
	if envelope.Error == nil || envelope.Error.Code == "" {
		t.Fatalf("failure needs a machine-readable code: %s", result)
	}
}

// Empty or absent arguments must be treated as {}, matching how the decoder
// tolerates missing input for no-argument tools such as jobs_list.
func TestRunToolAcceptsEmptyArguments(t *testing.T) {
	result, ok := (&Service{}).RunTool(context.Background(), "nope", "")
	if ok {
		t.Fatal("expected failure for unknown tool")
	}
	if !json.Valid([]byte(result)) {
		t.Fatalf("result must always be valid JSON: %s", result)
	}
}

// One analyze call on a long asset returns far more evidence than a model can
// use in a single step. Oversized results must be clamped so they cannot crowd
// the conversation out of the context window.
func TestClampToolResultKeepsPayloadBounded(t *testing.T) {
	// A result shaped like a real evidence dump, with multibyte runes so the cut
	// point has to respect UTF-8 boundaries.
	big := `{"api_version":"v1","ok":true,"result":{"evidence":[` + strings.Repeat(`{"transcript":"数学问题很难"},`, 4000) + `]}}`
	if len(big) <= maxToolResultBytes {
		t.Fatalf("fixture is not large enough: %d bytes", len(big))
	}

	got := clampToolResult(big)
	if len(got) > maxToolResultBytes+1024 {
		t.Fatalf("clamped result still too large: %d bytes", len(got))
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("clamped result must stay valid JSON so the model can read it")
	}
	var decoded struct {
		Truncated bool   `json:"truncated"`
		Notice    string `json:"notice"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Truncated || decoded.Notice == "" {
		t.Fatalf("truncation must be explained to the model: %s", got[:200])
	}
}

func TestClampToolResultLeavesSmallResultsAlone(t *testing.T) {
	small := `{"api_version":"v1","ok":true,"result":{"id":"proj-1"}}`
	if got := clampToolResult(small); got != small {
		t.Fatalf("small results must pass through untouched:\n got %s\nwant %s", got, small)
	}
}

// Concurrency classification is a safety property: a tool that writes must never
// be dispatched alongside another call, or revision numbers and generated ids
// become nondeterministic.
func TestOnlyObservingToolsAreConcurrencySafe(t *testing.T) {
	stateful := map[string]bool{
		"project_create": true, "assets_import": true, "analyze": true,
		"evidence_add": true, "timeline_create": true, "proposal_create": true,
		"edit_apply": true, "render_submit": true, "jobs_cancel": true,
	}
	for _, spec := range (&Service{}).ToolSpecs() {
		if stateful[spec.Name] && spec.ConcurrencySafe() {
			t.Errorf("%s mutates state and must not be concurrency-safe", spec.Name)
		}
		if !stateful[spec.Name] && !spec.ConcurrencySafe() {
			t.Errorf("%s only observes but is not marked ReadOnly", spec.Name)
		}
	}
}
