// Test-only bridges; no production code under test.

package control

import (
	"testing"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// These bridges exist only in the root package's test build, allowing external
// adapter integration tests to exercise the shared monitor without an import cycle.
func ControlWorkerForTest(mode string) int { return controlWorkerHelper(mode) }
func NewPreparedRunForTest(t *testing.T, plan Plan, coreIDs []int32, startIssued bool, adapters ...TaskAdapter) *Run {
	t.Helper()
	return newPreparedTypedHarness(t, plan, coreIDs, startIssued, adapters...)
}
func ObserveRunForTest(r *Run, message protocol.Message) { r.observe(message) }
func ObserveTypedForTest(r *Run, callback protocol.Callback, taskID *int32, malformed error) {
	r.observeTyped(callback, taskID, malformed)
}

type RunInspectionForTest struct {
	Fatal           string
	Proof           State
	ProofCode       string
	TypedEventCount int
	TypedEventBytes int
}

func InspectRunForTest(r *Run) RunInspectionForTest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RunInspectionForTest{r.fatal, r.proof, r.proofCode, r.typedEventCount, r.typedEventBytes}
}
func RunRecordDirForTest(r *Run) string { return r.runtime.store.dir }
func ApplyTypedUpdateForTest(result *TypedResult, update TypedUpdate) (*TypedResult, bool, error) {
	return applyTypedUpdate(result, update)
}
func ValidateTypedResultForTest(result *TypedResult) error  { return validateTypedResult(result) }
func TypedStateForTest(result *TypedResult) (State, string) { return typedState(result) }
func PreparedReducersForTest(plan PreparedPlan) map[int]TaskReducer {
	reducers := make(map[int]TaskReducer, len(plan.reducers))
	for index, reducer := range plan.reducers {
		reducers[index] = reducer
	}
	return reducers
}
func SameTargetForTest(a, b TargetResult) bool { return sameTarget(a, b) }
func NewRunIDForTest() string                  { return newRunID() }
func ClonePlanForTest(plan Plan) Plan          { return clonePlan(plan) }
func TerminalStateForTest(state State) bool    { return terminal(state) }

type RecordStoreForTest struct{ store *store }

func OpenRecordStoreForTest(dir string) (*RecordStoreForTest, error) {
	s, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	return &RecordStoreForTest{store: s}, nil
}
func (s *RecordStoreForTest) Save(record Record) error { return s.store.save(record) }
func (s *RecordStoreForTest) Close() error             { return s.store.close() }

// Shared harness bridges. The external test package aliases these instead of
// keeping a second copy of the runtime fixtures.
func RuntimePlanForTest(t *testing.T) Plan                         { return runtimePlan(t) }
func RuntimeConfigForTest(t *testing.T, mode string) Config        { return runtimeConfig(t, mode) }
func NewRuntimeForTest(t *testing.T, cfg Config) *Runtime          { return newRuntime(t, cfg) }
func WaitRuntimeRunForTest(t *testing.T, r *Run) ([]Event, Result) { return waitRuntimeRun(t, r) }
func WaitStartedForTest(t *testing.T, r *Run)                      { waitStarted(t, r) }
func ValidPlanForTest(t *testing.T) Plan                           { return validTestPlan(t) }
func RequireWorkerProcessExitedForTest(t *testing.T, pidFile string) {
	requireWorkerProcessExited(t, pidFile)
}
func SendTypedCallbackForTest(t *testing.T, r *Run, id int32, raw string) {
	sendTypedCallback(t, r, id, raw)
}
func TypedEventsInForTest(r *Run) []Event { return typedEventsIn(r) }
