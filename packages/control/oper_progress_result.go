package control

// operProgressReducer interprets callbacks for one prepared OperProgress task.
// The runtime observes it serially on its single monitor goroutine, so it needs
// no internal locking. Scratch state must stay bounded and must not retain
// Observation.JSON.
type operProgressReducer struct {
	targets []operProgressTarget
}

// newOperProgressReducer binds a fresh reducer to a detached copy of the target
// mapping prepared for exactly one task.
func newOperProgressReducer(targets []operProgressTarget) TaskReducer {
	return &operProgressReducer{targets: append([]operProgressTarget(nil), targets...)}
}

// Observe reports no business fact yet. Prepared targets remain unobserved and
// the typed aggregate stays incomplete until target interpretation is added; the
// placeholder must never manufacture success.
func (r *operProgressReducer) Observe(Observation) (TypedUpdate, error) {
	return TypedUpdate{}, nil
}
