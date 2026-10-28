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

// PendingTurnIDForTest reports the controller's in-memory pending provider turn
// id. Tests use it to observe the dispatch ordering at the archive boundary.
func (c *Controller) PendingTurnIDForTest() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingTurnID
}

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

// SetDeferredPendingStopLimitsForTest shortens the two bounded finalization
// windows and returns a restore function.
func (c *Controller) SetDeferredPendingStopLimitsForTest(wait, cleanup time.Duration) (restore func()) {
	c.mu.Lock()
	previousWait := c.deferredPendingStopWaitOverride
	previousCleanup := c.deferredPendingStopCleanupOverride
	c.deferredPendingStopWaitOverride = wait
	c.deferredPendingStopCleanupOverride = cleanup
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.deferredPendingStopWaitOverride = previousWait
		c.deferredPendingStopCleanupOverride = previousCleanup
		c.mu.Unlock()
	}
}

// DeferredDispatchTrackingForTest reports the in-memory tracking a failed
// finalization must retain.
func (c *Controller) DeferredDispatchTrackingForTest() (dispatchingTurnID, providerTurnID, phase string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dispatchingTurnID, c.deferredProviderTurnID, c.deferredPhase.String()
}
