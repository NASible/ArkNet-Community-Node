package engine

// Reporter is the worker-originated mock report boundary. It grants no ArkNet
// permissions, performs no real verification, and never calls back to an Agent.
type Reporter interface {
	ReportProgress(id string, completedBytes int64) (Status, error)
	ReportCompletion(id string) (Status, error)
	ReportVerificationFailure(id string) (Status, error)
}

func (e *Engine) ReportProgress(id string, completedBytes int64) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.entries[id]
	if !ok {
		return Status{}, ErrNotFound
	}
	e.refresh(v)
	if v.status.State != Downloading {
		return Status{}, ErrTransition
	}
	if completedBytes < v.status.CompletedBytes || completedBytes > v.status.TotalBytes {
		return Status{}, ErrInvalid
	}
	v.status.CompletedBytes = completedBytes
	return v.status, nil
}
func (e *Engine) ReportCompletion(id string) (Status, error) { return e.reportTerminal(id, Completed) }
func (e *Engine) ReportVerificationFailure(id string) (Status, error) {
	return e.reportTerminal(id, VerificationFailed)
}
func (e *Engine) reportTerminal(id string, target State) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.entries[id]
	if !ok {
		return Status{}, ErrNotFound
	}
	e.refresh(v)
	if v.status.State == target {
		return v.status, nil
	}
	if v.status.State != Downloading && v.status.State != Paused {
		return Status{}, ErrTransition
	}
	v.status.State = target
	if target == Completed {
		v.status.CompletedBytes = v.status.TotalBytes
	} else {
		v.status.ErrorCode = "verification_failed"
	}
	return v.status, nil
}
