package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func operProgressTask(params string) Task {
	return Task{ID: "grow", Type: operProgressTaskType, Params: json.RawMessage(params)}
}

func operProgressPlansJSON(n int) string {
	plans := make([]string, n)
	for i := range plans {
		plans[i] = fmt.Sprintf(`{"role":"Warrior","name":"operator-%03d","elite":1}`, i)
	}
	return `{"plans":[` + strings.Join(plans, ",") + `]}`
}

func TestOperProgressAdapterDescriptor(t *testing.T) {
	adapter := NewOperProgressAdapter()
	want := AdapterRef{Name: "oper_progress", Version: 1}
	if got := adapter.Descriptor(); got != want {
		t.Fatalf("descriptor = %+v, want %+v", got, want)
	}
}

func TestOperProgressPrepareCompilesParamsAndTargets(t *testing.T) {
	adapter := NewOperProgressAdapter()
	prepared, err := adapter.Prepare(operProgressTask(`{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_level":7},{"role":"Medic","name":"Gamma","skill_mastery":[0,2,0]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	const wantParams = `{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_level":7},{"role":"Medic","name":"Gamma","skill_mastery":[0,2,0]}]}`
	if string(prepared.Params) != wantParams {
		t.Fatalf("compiled params = %s, want %s", prepared.Params, wantParams)
	}
	wantTargets := []string{"op-0001", "op-0002", "op-0003"}
	if !reflect.DeepEqual(prepared.Targets, wantTargets) {
		t.Fatalf("targets = %v, want %v", prepared.Targets, wantTargets)
	}
	reducer, ok := prepared.Reducer.(*operProgressReducer)
	if !ok || reducer == nil {
		t.Fatalf("reducer has type %T, want fresh *operProgressReducer", prepared.Reducer)
	}
	wantMapping := []operProgressTarget{
		{ID: "op-0001", SourceIndex: 0, Role: "Warrior", Name: "Alpha", Action: operProgressActionElite, Expected: 2, MasterySlot: 0},
		{ID: "op-0002", SourceIndex: 1, Role: "Caster", Name: "Beta", Action: operProgressActionSkillLevel, Expected: 7, MasterySlot: 0},
		{ID: "op-0003", SourceIndex: 2, Role: "Medic", Name: "Gamma", Action: operProgressActionSkillMastery, Expected: 2, MasterySlot: 2},
	}
	if !reflect.DeepEqual(reducer.targets, wantMapping) {
		t.Fatalf("target mapping = %+v, want %+v", reducer.targets, wantMapping)
	}
	if update, err := reducer.Observe(Observation{MessageID: 20003}); err != nil || len(update.Targets) != 0 || len(update.Summary) != 0 {
		t.Fatalf("placeholder reducer invented a fact: %+v, %v", update, err)
	}
}

func TestOperProgressPrepareAcceptsActionBoundaries(t *testing.T) {
	adapter := NewOperProgressAdapter()
	longName := strings.Repeat("a", operProgressMaxNameBytes)
	cases := []struct {
		name    string
		params  string
		targets int
	}{
		{"elite minimum", `{"plans":[{"role":"Pioneer","name":"A","elite":1}]}`, 1},
		{"elite maximum", `{"plans":[{"role":"Warrior","name":"A","elite":2}]}`, 1},
		{"skill level minimum", `{"plans":[{"role":"Tank","name":"A","skill_level":2}]}`, 1},
		{"skill level maximum", `{"plans":[{"role":"Sniper","name":"A","skill_level":7}]}`, 1},
		{"mastery first slot", `{"plans":[{"role":"Caster","name":"A","skill_mastery":[1,0,0]}]}`, 1},
		{"mastery middle slot", `{"plans":[{"role":"Medic","name":"A","skill_mastery":[0,2,0]}]}`, 1},
		{"mastery last slot", `{"plans":[{"role":"Support","name":"A","skill_mastery":[0,0,3]}]}`, 1},
		{"special role", `{"plans":[{"role":"Special","name":"A","elite":1}]}`, 1},
		{"name maximum bytes", `{"plans":[{"role":"Medic","name":"` + longName + `","elite":1}]}`, 1},
		{"name whitespace preserved", `{"plans":[{"role":"Warrior","name":" A ","elite":1}]}`, 1},
		{"name unicode preserved", `{"plans":[{"role":"Warrior","name":"示例干员甲","elite":1}]}`, 1},
		{"same name different roles", `{"plans":[{"role":"Warrior","name":"A","elite":1},{"role":"Tank","name":"A","elite":1}]}`, 2},
		{"same role different names", `{"plans":[{"role":"Warrior","name":"A","elite":1},{"role":"Warrior","name":"B","elite":2}]}`, 2},
		{"plans keep input order", `{"plans":[{"role":"Medic","name":"Third","skill_level":3},{"role":"Warrior","name":"First","elite":1},{"role":"Caster","name":"Second","skill_mastery":[0,0,1]}]}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := adapter.Prepare(operProgressTask(tc.params))
			if err != nil {
				t.Fatal(err)
			}
			if string(prepared.Params) != tc.params {
				t.Fatalf("compiled params = %s, want %s", prepared.Params, tc.params)
			}
			if len(prepared.Targets) != tc.targets {
				t.Fatalf("got %d targets, want %d", len(prepared.Targets), tc.targets)
			}
			if prepared.Reducer == nil {
				t.Fatal("adapter returned a nil reducer")
			}
		})
	}
}

func TestOperProgressPrepareRejectsInvalidInput(t *testing.T) {
	adapter := NewOperProgressAdapter()
	valid := `{"plans":[{"role":"Warrior","name":"A","elite":1}]}`
	cases := []struct {
		name   string
		params string
	}{
		{"params null", `null`},
		{"params array", `[]`},
		{"params scalar", `"plans"`},
		{"params empty object", `{}`},
		{"plans null", `{"plans":null}`},
		{"plans object", `{"plans":{}}`},
		{"plans string", `{"plans":"x"}`},
		{"plans empty", `{"plans":[]}`},
		{"unknown top-level field", `{"plans":[{"role":"Warrior","name":"A","elite":1}],"enable":true}`},
		{"duplicate top-level key", `{"plans":[{"role":"Warrior","name":"A","elite":1}],"plans":[{"role":"Tank","name":"B","elite":2}]}`},
		{"trailing JSON value", valid + ` {}`},
		{"invalid UTF-8", valid + string([]byte{0xff})},
		{"plan is null", `{"plans":[null]}`},
		{"plan is array", `{"plans":[[]]}`},
		{"plan is string", `{"plans":["x"]}`},
		{"unknown plan field", `{"plans":[{"role":"Warrior","name":"A","elite":1,"enable":true}]}`},
		{"duplicate plan key", `{"plans":[{"role":"Warrior","role":"Tank","name":"A","elite":1}]}`},
		{"nested duplicate key", `{"plans":[{"role":"Warrior","name":"A","elite":1,"future":{"x":1,"x":2}}]}`},
		{"null role", `{"plans":[{"role":null,"name":"A","elite":1}]}`},
		{"null name", `{"plans":[{"role":"Warrior","name":null,"elite":1}]}`},
		{"null elite", `{"plans":[{"role":"Warrior","name":"A","elite":null}]}`},
		{"null skill level", `{"plans":[{"role":"Warrior","name":"A","skill_level":null}]}`},
		{"null mastery", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":null}]}`},
		{"missing role", `{"plans":[{"name":"A","elite":1}]}`},
		{"missing name", `{"plans":[{"role":"Warrior","elite":1}]}`},
		{"role number", `{"plans":[{"role":1,"name":"A","elite":1}]}`},
		{"role bool", `{"plans":[{"role":true,"name":"A","elite":1}]}`},
		{"role array", `{"plans":[{"role":[],"name":"A","elite":1}]}`},
		{"role object", `{"plans":[{"role":{},"name":"A","elite":1}]}`},
		{"role unknown", `{"plans":[{"role":"Unknown","name":"A","elite":1}]}`},
		{"role drone", `{"plans":[{"role":"Drone","name":"A","elite":1}]}`},
		{"role lowercase", `{"plans":[{"role":"warrior","name":"A","elite":1}]}`},
		{"role trailing space", `{"plans":[{"role":"Warrior ","name":"A","elite":1}]}`},
		{"role empty", `{"plans":[{"role":"","name":"A","elite":1}]}`},
		{"name number", `{"plans":[{"role":"Warrior","name":5,"elite":1}]}`},
		{"name bool", `{"plans":[{"role":"Warrior","name":true,"elite":1}]}`},
		{"name empty", `{"plans":[{"role":"Warrior","name":"","elite":1}]}`},
		{"name spaces", `{"plans":[{"role":"Warrior","name":"   ","elite":1}]}`},
		{"name tab", `{"plans":[{"role":"Warrior","name":"\t","elite":1}]}`},
		{"name unicode space", `{"plans":[{"role":"Warrior","name":"\u3000","elite":1}]}`},
		{"name NUL", `{"plans":[{"role":"Warrior","name":"A\u0000B","elite":1}]}`},
		{"name over 256 bytes", `{"plans":[{"role":"Warrior","name":"` + strings.Repeat("a", operProgressMaxNameBytes+1) + `","elite":1}]}`},
		{"no action", `{"plans":[{"role":"Warrior","name":"A"}]}`},
		{"elite and skill level", `{"plans":[{"role":"Warrior","name":"A","elite":1,"skill_level":2}]}`},
		{"elite and mastery", `{"plans":[{"role":"Warrior","name":"A","elite":1,"skill_mastery":[1,0,0]}]}`},
		{"skill level and mastery", `{"plans":[{"role":"Warrior","name":"A","skill_level":2,"skill_mastery":[1,0,0]}]}`},
		{"all actions", `{"plans":[{"role":"Warrior","name":"A","elite":1,"skill_level":2,"skill_mastery":[1,0,0]}]}`},
		{"elite zero", `{"plans":[{"role":"Warrior","name":"A","elite":0}]}`},
		{"elite three", `{"plans":[{"role":"Warrior","name":"A","elite":3}]}`},
		{"elite fractional", `{"plans":[{"role":"Warrior","name":"A","elite":1.5}]}`},
		{"elite exponent", `{"plans":[{"role":"Warrior","name":"A","elite":1e0}]}`},
		{"elite string", `{"plans":[{"role":"Warrior","name":"A","elite":"1"}]}`},
		{"elite bool", `{"plans":[{"role":"Warrior","name":"A","elite":true}]}`},
		{"elite array", `{"plans":[{"role":"Warrior","name":"A","elite":[1]}]}`},
		{"skill level one", `{"plans":[{"role":"Warrior","name":"A","skill_level":1}]}`},
		{"skill level eight", `{"plans":[{"role":"Warrior","name":"A","skill_level":8}]}`},
		{"skill level fractional", `{"plans":[{"role":"Warrior","name":"A","skill_level":2.5}]}`},
		{"skill level string", `{"plans":[{"role":"Warrior","name":"A","skill_level":"2"}]}`},
		{"mastery length two", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0]}]}`},
		{"mastery length four", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,0,0]}]}`},
		{"mastery all zero", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,0]}]}`},
		{"mastery two nonzero", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[1,2,0]}]}`},
		{"mastery level four", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,4]}]}`},
		{"mastery negative", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,-1]}]}`},
		{"mastery fractional", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,1.5]}]}`},
		{"mastery exponent", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,1e0]}]}`},
		{"mastery string element", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,"1"]}]}`},
		{"mastery null element", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[0,0,null]}]}`},
		{"mastery object", `{"plans":[{"role":"Warrior","name":"A","skill_mastery":{}}]}`},
		{"duplicate role and name", `{"plans":[{"role":"Warrior","name":"A","elite":1},{"role":"Warrior","name":"A","skill_level":2}]}`},
		{"duplicate exact plan", `{"plans":[{"role":"Warrior","name":"A","elite":1},{"role":"Warrior","name":"A","elite":1}]}`},
		{"too many plans", operProgressPlansJSON(operProgressMaxPlans + 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.Prepare(operProgressTask(tc.params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("invalid input accepted: %v", err)
			}
		})
	}
}

func TestOperProgressPrepareRejectsWrongCoreType(t *testing.T) {
	adapter := NewOperProgressAdapter()
	// The params are otherwise acceptable so only the Core type check can reject.
	params := json.RawMessage(`{"plans":[{"role":"Warrior","name":"A","elite":1}]}`)
	for _, taskType := range []string{"SwitchTheme", "oper_progress", "OperProgress ", ""} {
		if _, err := adapter.Prepare(Task{ID: "grow", Type: taskType, Params: params}); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("Core type %q accepted: %v", taskType, err)
		}
	}
}

func TestOperProgressRejectsCaseVariantFields(t *testing.T) {
	adapter := NewOperProgressAdapter()
	validPlan := `{"role":"Warrior","name":"A","elite":1}`
	for name, params := range map[string]string{
		"upper plans":                    `{"PLANS":[` + validPlan + `]}`,
		"title plans":                    `{"Plans":[` + validPlan + `]}`,
		"plans and title plans":          `{"plans":[` + validPlan + `],"Plans":[{"role":"Tank","name":"B","skill_level":2}]}`,
		"upper role":                     `{"plans":[{"Role":"Warrior","name":"A","elite":1}]}`,
		"role and upper role":            `{"plans":[{"role":"Warrior","Role":"Tank","name":"A","elite":1}]}`,
		"upper name":                     `{"plans":[{"role":"Warrior","Name":"A","elite":1}]}`,
		"upper elite":                    `{"plans":[{"role":"Warrior","name":"A","ELITE":1}]}`,
		"elite and upper elite":          `{"plans":[{"role":"Warrior","name":"A","elite":1,"Elite":2}]}`,
		"title skill level":              `{"plans":[{"role":"Warrior","name":"A","Skill_Level":2}]}`,
		"title skill mastery":            `{"plans":[{"role":"Warrior","name":"A","Skill_Mastery":[1,0,0]}]}`,
		"mastery and title mastery":      `{"plans":[{"role":"Warrior","name":"A","skill_mastery":[1,0,0],"Skill_Mastery":[0,0,2]}]}`,
		"upper role beside valid fields": `{"plans":[{"ROLE":"Warrior","name":"A","elite":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.Prepare(operProgressTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("case-variant field accepted: %v", err)
			}
		})
	}
}

func TestOperProgressRejectsUnpairedSurrogateNames(t *testing.T) {
	adapter := NewOperProgressAdapter()
	for name, params := range map[string]string{
		"high surrogate":             `{"plans":[{"role":"Warrior","name":"\ud800","elite":1}]}`,
		"low surrogate":              `{"plans":[{"role":"Warrior","name":"\udc00","elite":1}]}`,
		"high then plain":            `{"plans":[{"role":"Warrior","name":"\ud800\u0041","elite":1}]}`,
		"two high surrogates":        `{"plans":[{"role":"Warrior","name":"\ud800\ud800","elite":1}]}`,
		"high surrogate in role":     `{"plans":[{"role":"\ud800","name":"A","elite":1}]}`,
		"high surrogate in plan key": `{"plans":[{"role":"Warrior","name":"A","\ud800":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.Prepare(operProgressTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("unpaired surrogate accepted: %v", err)
			}
		})
	}
	// A correctly paired surrogate escape and a literal replacement-character
	// escape name the same characters the caller asked for.
	for _, tc := range []struct{ in, want string }{
		{`{"plans":[{"role":"Warrior","name":"\ud83d\ude00","elite":1}]}`, `{"plans":[{"role":"Warrior","name":"😀","elite":1}]}`},
		{`{"plans":[{"role":"Warrior","name":"\ufffd","elite":1}]}`, `{"plans":[{"role":"Warrior","name":"�","elite":1}]}`},
	} {
		prepared, err := adapter.Prepare(operProgressTask(tc.in))
		if err != nil {
			t.Fatalf("valid surrogate escape %s rejected: %v", tc.in, err)
		}
		if string(prepared.Params) != tc.want {
			t.Fatalf("decoded name = %s, want %s", prepared.Params, tc.want)
		}
	}
}

func TestOperProgressPreparePlanCountBoundary(t *testing.T) {
	adapter := NewOperProgressAdapter()
	prepared, err := adapter.Prepare(operProgressTask(operProgressPlansJSON(operProgressMaxPlans)))
	if err != nil {
		t.Fatalf("maximum plan count rejected: %v", err)
	}
	if len(prepared.Targets) != operProgressMaxPlans || prepared.Targets[0] != "op-0001" || prepared.Targets[operProgressMaxPlans-1] != "op-0128" {
		t.Fatalf("unexpected target sequence at the count boundary: %d targets", len(prepared.Targets))
	}
	if _, err := adapter.Prepare(operProgressTask(operProgressPlansJSON(operProgressMaxPlans + 1))); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("plan count over the limit accepted: %v", err)
	}
}

func TestOperProgressPrepareParamsByteBoundary(t *testing.T) {
	adapter := NewOperProgressAdapter()
	base := `{"plans":[{"role":"Warrior","name":"A","elite":1}]}`
	for _, size := range []int{MaxAdapterParamsBytes, MaxAdapterParamsBytes + 1} {
		params := base + strings.Repeat(" ", size-len(base))
		_, err := adapter.Prepare(operProgressTask(params))
		if size == MaxAdapterParamsBytes && err != nil {
			t.Fatalf("params at the byte limit rejected: %v", err)
		}
		if size > MaxAdapterParamsBytes && !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("params over the byte limit accepted: %v", err)
		}
	}
}

func TestOperProgressPrepareRejectsCompiledParamsOverBudget(t *testing.T) {
	// Escaping expands literal '<' sixfold in compiled JSON, so a structurally
	// valid input can compile past the per-task Core params budget.
	name := strings.Repeat("<", operProgressMaxNameBytes)
	plans := make([]string, operProgressMaxPlans)
	for i := range plans {
		plans[i] = `{"role":"Warrior","name":"` + name + `","elite":1}`
	}
	params := `{"plans":[` + strings.Join(plans, ",") + `]}`
	adapter := NewOperProgressAdapter()
	if _, err := adapter.Prepare(operProgressTask(params)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("compiled params over the byte limit accepted: %v", err)
	}
}

func TestOperProgressPrepareIsDetachedAndFreshPerTask(t *testing.T) {
	adapter := NewOperProgressAdapter()
	const source = `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`
	const wantParams = `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`
	input := []byte(source)
	prepared, err := adapter.Prepare(Task{ID: "grow-1", Type: operProgressTaskType, Params: json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != source {
		t.Fatal("Prepare mutated the caller's params")
	}
	reducer := prepared.Reducer.(*operProgressReducer)
	input[2] = 'X'
	if string(prepared.Params) != wantParams || prepared.Targets[0] != "op-0001" || reducer.targets[0].Name != "Alpha" {
		t.Fatal("prepared output aliases the caller's params")
	}

	second, err := adapter.Prepare(operProgressTask(source))
	if err != nil {
		t.Fatal(err)
	}
	secondReducer := second.Reducer.(*operProgressReducer)
	if secondReducer == reducer || &secondReducer.targets[0] == &reducer.targets[0] {
		t.Fatal("reducer was reused across preparations")
	}
	prepared.Targets[0] = "mutated"
	prepared.Params[2] = 'Y'
	reducer.targets[0].Name = "mutated"
	if second.Targets[0] != "op-0001" || string(second.Params) != wantParams || secondReducer.targets[0].Name != "Alpha" {
		t.Fatal("preparations share mutable slices or JSON buffers")
	}
}

func TestOperProgressRawPlanRemainsOpen(t *testing.T) {
	plan := validTestPlan(t)
	ref := AdapterRef{Name: "oper_progress", Version: 1}
	rawParams := json.RawMessage(`{"plans":[{"role":"Warrior","name":"A","elite":2}],"future":{"n":9007199254740993}}`)
	plan.Tasks = []Task{
		{ID: "raw", Type: "OperProgress", Params: rawParams},
		{ID: "strict", Type: "OperProgress", Adapter: &ref, Params: json.RawMessage(`{"plans":[{"role":"Caster","name":"B","skill_level":3}]}`)},
	}
	prepared, err := PreparePlan(plan, NewOperProgressAdapter())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Tasks[0].Typed != nil {
		t.Fatal("raw task was routed through the adapter")
	}
	if !bytes.Contains(prepared.Plan.Tasks[0].Params, []byte("9007199254740993")) || !bytes.Contains(prepared.Plan.Tasks[0].Params, []byte(`"future"`)) {
		t.Fatalf("raw params lost unknown fields or integer literals: %s", prepared.Plan.Tasks[0].Params)
	}
	typed := prepared.Tasks[1].Typed
	if typed == nil || len(typed.Targets) != 1 || typed.Targets[0].ID != "op-0001" || typed.Targets[0].Outcome != TargetUnobserved {
		t.Fatalf("strict task was not prepared: %+v", typed)
	}
	if !bytes.Contains(prepared.Plan.Tasks[1].Params, []byte(`"skill_level":3`)) {
		t.Fatalf("strict task kept uncompiled params: %s", prepared.Plan.Tasks[1].Params)
	}
}

func ExampleNewOperProgressAdapter() {
	adapter := NewOperProgressAdapter()
	prepared, err := adapter.Prepare(Task{
		ID:     "grow-1",
		Type:   "OperProgress",
		Params: json.RawMessage(`{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}`),
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	ref := adapter.Descriptor()
	fmt.Printf("%s v%d\n", ref.Name, ref.Version)
	fmt.Println(string(prepared.Params))
	fmt.Println(prepared.Targets)
	// Output:
	// oper_progress v1
	// {"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}
	// [op-0001 op-0002]
}
