package chat

import "time"

// SetNativeHistoryLoadAttemptLimit shortens the per-attempt provider load bound
// for tests and returns a restore function.
func SetNativeHistoryLoadAttemptLimit(limit time.Duration) (restore func()) {
	previous := nativeHistoryLoadAttemptLimit
	nativeHistoryLoadAttemptLimit = limit
	return func() { nativeHistoryLoadAttemptLimit = previous }
}

// SetRenderMeasureTimeout shortens how long publishing waits for heights, for
// tests, and returns a restore function.
func SetRenderMeasureTimeout(timeout time.Duration) (restore func()) {
	previous := renderMeasureTimeout
	renderMeasureTimeout = timeout
	return func() { renderMeasureTimeout = previous }
}

// WaitRenderMeasures waits for the background measures PublishRender started.
func (s *Service) WaitRenderMeasures() { s.renderMeasures.Wait() }

// ResumeHoldTurnIDForTest reports the turn id of the armed explicit-resume
// one-shot, or "" when none is armed. The marker is otherwise unexported.
func (c *Controller) ResumeHoldTurnIDForTest() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resumeHold == nil {
		return ""
	}
	return c.resumeHold.turnID
}

// RetainedQueueTurnIDForTest reports the turn id that produced the manual-resume
// retention, or "" when the queue is not retained.
func (c *Controller) RetainedQueueTurnIDForTest() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retainedQueue == nil {
		return ""
	}
	return c.retainedQueue.turnID
}
