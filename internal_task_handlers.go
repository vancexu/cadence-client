// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cadence

// All code in this file is private to the package.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/uber-go/tally"
	"go.uber.org/cadence/.gen/go/cadence/workflowserviceclient"
	s "go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/common"
	"go.uber.org/cadence/common/backoff"
	"go.uber.org/cadence/common/cache"
	"go.uber.org/cadence/common/metrics"
	"go.uber.org/cadence/common/util"
	"go.uber.org/zap"
)

const (
	defaultHeartBeatIntervalInSec = 10 * 60

	defaultStickyCacheSize = 10000
)

type (
	// workflowExecutionEventHandler process a single event.
	workflowExecutionEventHandler interface {
		// Process a single event and return the assosciated decisions.
		// Return List of decisions made, any error.
		ProcessEvent(event *s.HistoryEvent, isReplay bool, isLast bool) ([]*s.Decision, error)
		// ProcessQuery process a query request.
		ProcessQuery(queryType string, queryArgs []byte) ([]byte, error)
		StackTrace() string
		// Close for cleaning up resources on this event handler
		Close()
	}

	// workflowTask wraps a decision task.
	workflowTask struct {
		task            *s.PollForDecisionTaskResponse
		historyIterator HistoryIterator
		pollStartTime   time.Time
	}

	// activityTask wraps a activity task.
	activityTask struct {
		task          *s.PollForActivityTaskResponse
		pollStartTime time.Time
	}

	// workflowExecutionContext is the cached workflow state for sticky execution
	workflowExecutionContext struct {
		sync.Mutex
		workflowStartTime time.Time
		runID             string
		workflowInfo      *WorkflowInfo
		wth               *workflowTaskHandlerImpl

		eventHandler workflowExecutionEventHandler

		isWorkflowCompleted bool
		result              []byte
		err                 error

		previousStartedEventID int64
	}

	// workflowTaskHandlerImpl is the implementation of WorkflowTaskHandler
	workflowTaskHandlerImpl struct {
		domain                 string
		metricsScope           tally.Scope
		ppMgr                  pressurePointMgr
		logger                 *zap.Logger
		identity               string
		enableLoggingInReplay  bool
		disableStickyExecution bool
		hostEnv                *hostEnvImpl
	}

	activityProvider func(name string) activity
	// activityTaskHandlerImpl is the implementation of ActivityTaskHandler
	activityTaskHandlerImpl struct {
		taskListName     string
		identity         string
		service          workflowserviceclient.Interface
		metricsScope     tally.Scope
		logger           *zap.Logger
		userContext      context.Context
		hostEnv          *hostEnvImpl
		activityProvider activityProvider
	}

	// history wrapper method to help information about events.
	history struct {
		workflowTask      *workflowTask
		eventsHandler     *workflowExecutionEventHandlerImpl
		loadedEvents      []*s.HistoryEvent
		nextPageToken     []byte
		currentIndex      int
		historyEventsSize int
		next              []*s.HistoryEvent
	}
)

func newHistory(task *workflowTask, eventsHandler *workflowExecutionEventHandlerImpl) *history {
	result := &history{
		workflowTask:      task,
		eventsHandler:     eventsHandler,
		loadedEvents:      task.task.History.Events,
		currentIndex:      0,
		historyEventsSize: len(task.task.History.Events),
	}

	return result
}

// Get workflow start event.
func (eh *history) GetWorkflowStartedEvent() (*s.HistoryEvent, error) {
	events := eh.workflowTask.task.History.Events
	if len(events) == 0 || events[0].GetEventType() != s.EventTypeWorkflowExecutionStarted {
		return nil, errors.New("unable to find WorkflowExecutionStartedEventAttributes in the history")
	}
	return events[0], nil
}

// Get last non replayed event ID.
func (eh *history) LastNonReplayedID() int64 {
	return eh.workflowTask.task.GetPreviousStartedEventId()
}

func (eh *history) IsNextDecisionFailed() bool {
	events := eh.workflowTask.task.History.Events
	eventsSize := len(events)
	for i := eh.currentIndex; i < eventsSize; i++ {
		switch events[i].GetEventType() {
		case s.EventTypeDecisionTaskCompleted:
			return false
		case s.EventTypeDecisionTaskTimedOut:
			return true
		case s.EventTypeDecisionTaskFailed:
			return true
		}
	}
	return false
}

func isDecisionEvent(eventType s.EventType) bool {
	switch eventType {
	case s.EventTypeWorkflowExecutionCompleted,
		s.EventTypeWorkflowExecutionFailed,
		s.EventTypeWorkflowExecutionCanceled,
		s.EventTypeWorkflowExecutionContinuedAsNew,
		s.EventTypeActivityTaskScheduled,
		s.EventTypeActivityTaskCancelRequested,
		s.EventTypeTimerStarted,
		s.EventTypeTimerCanceled,
		s.EventTypeMarkerRecorded,
		s.EventTypeStartChildWorkflowExecutionInitiated,
		s.EventTypeRequestCancelExternalWorkflowExecutionInitiated:
		return true
	default:
		return false
	}
}

// NextDecisionEvents returns events that there processed as new by the next decision.
// It also reorders events that were added to a history during outgoing decision. Without
// such reordering determinism is broken.
// For Ex: (pseudo code)
//   ResultA := Schedule_Activity_A
//   ResultB := Schedule_Activity_B
//   if ResultB.IsReady() { panic error }
//   ResultC := Schedule_Activity_C(ResultA)
// If both A and B activities complete then we could have two different paths, Either Scheduling C (or) Panic'ing.
// Workflow events:
// 	Workflow_Start, DecisionStart1, DecisionComplete1, A_Schedule, B_Schedule, A_Complete,
//      DecisionStart2, B_Complete, DecisionComplete2, C_Schedule.
// B_Complete happened concurrent to execution of the decision(2), where C_Schedule is a result made
// by execution of decision(2).
// To maintain determinism the concurrent decisions are moved to the one after the decisions made by current decision.
// markers result value returns marker events that currently running decision produced. They are used to
// implement SideEffect method execution without blocking on decision roundtrip.
func (eh *history) NextDecisionEvents() (result []*s.HistoryEvent, markers []*s.HistoryEvent, err error) {
	if eh.next == nil {
		eh.next, _, err = eh.nextDecisionEvents()
		if err != nil {
			return result, markers, err
		}
	}

	result = eh.next
	if len(result) > 0 {
		// Set replay clock.
		lastEvent := result[len(result)-1]
		if lastEvent.GetEventType() == s.EventTypeDecisionTaskStarted {
			ts := time.Unix(0, lastEvent.GetTimestamp())
			eh.eventsHandler.SetCurrentReplayTime(ts)
		}

		eh.next, markers, err = eh.nextDecisionEvents()
	}
	return result, markers, err
}

func (eh *history) hasMoreEvents() bool {
	historyIterator := eh.workflowTask.historyIterator
	return historyIterator != nil && historyIterator.HasNextPage()
}

func (eh *history) getMoreEvents() (*s.History, error) {
	return eh.workflowTask.historyIterator.GetNextPage()
}

func (eh *history) nextDecisionEvents() (reorderedEvents []*s.HistoryEvent, markers []*s.HistoryEvent, err error) {
	if eh.currentIndex == len(eh.loadedEvents) && !eh.hasMoreEvents() {
		return []*s.HistoryEvent{}, []*s.HistoryEvent{}, nil
	}

	// Process events

	decisionStartToCompletionEvents := []*s.HistoryEvent{}
	decisionCompletionToStartEvents := []*s.HistoryEvent{}
	var decisionStartedEvent *s.HistoryEvent
	concurrentToDecision := true
	lastDecisionIndex := -1

OrderEvents:
	for {
		// load more history events if needed
		for eh.currentIndex == len(eh.loadedEvents) {
			if !eh.hasMoreEvents() {
				break OrderEvents
			}
			historyPage, err1 := eh.getMoreEvents()
			if err1 != nil {
				err = err1
				return
			}
			eh.loadedEvents = append(eh.loadedEvents, historyPage.Events...)
		}

		event := eh.loadedEvents[eh.currentIndex]
		switch event.GetEventType() {
		case s.EventTypeDecisionTaskStarted:
			if !eh.IsNextDecisionFailed() {
				eh.currentIndex++ // Since we already processed the current event
				decisionStartedEvent = event
				break OrderEvents
			}

		case s.EventTypeDecisionTaskCompleted:
			concurrentToDecision = false

		case s.EventTypeDecisionTaskScheduled, s.EventTypeDecisionTaskTimedOut, s.EventTypeDecisionTaskFailed:
		// Skip
		default:
			if concurrentToDecision {
				decisionStartToCompletionEvents = append(decisionStartToCompletionEvents, event)
			} else {
				if isDecisionEvent(event.GetEventType()) {
					lastDecisionIndex = len(decisionCompletionToStartEvents)
				}
				if event.GetEventType() == s.EventTypeMarkerRecorded {
					markers = append(markers, event)
				}
				decisionCompletionToStartEvents = append(decisionCompletionToStartEvents, event)
			}
		}
		eh.currentIndex++
	}

	// Reorder events to correspond to the order that decider sees them.
	// The main difference is that events that were added during decision task execution
	// should be processed after events that correspond to the decisions.
	// Otherwise the replay is going to break.

	// First are events that correspond to the previous task decisions
	if lastDecisionIndex >= 0 {
		// Make a copy of the slice.
		reorderedEvents = append(reorderedEvents, decisionCompletionToStartEvents[:lastDecisionIndex+1]...)
	}
	// Second are events that were added during previous task execution
	reorderedEvents = append(reorderedEvents, decisionStartToCompletionEvents...)
	// The last are events that were added after previous task completion
	if lastDecisionIndex+1 < len(decisionCompletionToStartEvents) {
		reorderedEvents = append(reorderedEvents, decisionCompletionToStartEvents[lastDecisionIndex+1:]...)
	}
	if decisionStartedEvent != nil {
		reorderedEvents = append(reorderedEvents, decisionStartedEvent)
	}
	return reorderedEvents, markers, nil
}

// newWorkflowTaskHandler returns an implementation of workflow task handler.
func newWorkflowTaskHandler(
	domain string,
	params workerExecutionParameters,
	ppMgr pressurePointMgr,
	hostEnv *hostEnvImpl,
) WorkflowTaskHandler {
	ensureRequiredParams(&params)
	return &workflowTaskHandlerImpl{
		domain:                 domain,
		logger:                 params.Logger,
		ppMgr:                  ppMgr,
		metricsScope:           params.MetricsScope,
		identity:               params.Identity,
		enableLoggingInReplay:  params.EnableLoggingInReplay,
		disableStickyExecution: params.DisableStickyExecution,
		hostEnv:                hostEnv,
	}
}

// TODO: need a better eviction policy based on memory usage
var workflowCache = cache.New(defaultStickyCacheSize, &cache.Options{
	RemovedFunc: func(cachedEntity interface{}) {
		wc := cachedEntity.(*workflowExecutionContext)
		wc.onEviction()
	},
})

func getWorkflowContext(runID string) *workflowExecutionContext {
	o := workflowCache.Get(runID)
	if o == nil {
		return nil
	}
	wc := o.(*workflowExecutionContext)
	return wc
}

func putWorkflowContext(runID string, wc *workflowExecutionContext) (*workflowExecutionContext, error) {
	existing, err := workflowCache.PutIfNotExist(runID, wc)
	if err != nil {
		return nil, err
	}
	return existing.(*workflowExecutionContext), nil
}

func removeWorkflowContext(runID string) {
	workflowCache.Delete(runID)
}

func (w *workflowExecutionContext) release() {
	w.Unlock()
}

func (w *workflowExecutionContext) completeWorkflow(result []byte, err error) {
	w.isWorkflowCompleted = true
	w.result = result
	w.err = err
}

func (w *workflowExecutionContext) onEviction() {
	// onEviction is run by LRU cache's removeFunc in separate goroutinue
	w.Lock()
	w.destroyCachedState()
	w.Unlock()
}

func (w *workflowExecutionContext) isDestroyed() bool {
	return w.eventHandler == nil
}

func (w *workflowExecutionContext) destroyCachedState() {
	w.isWorkflowCompleted = false
	w.result = nil
	w.err = nil
	w.previousStartedEventID = 0
	if w.eventHandler != nil {
		w.eventHandler.Close()
		w.eventHandler = nil
	}
}

func (w *workflowExecutionContext) resetWorkflowState() {
	w.destroyCachedState()
	w.eventHandler = newWorkflowExecutionEventHandler(
		w.workflowInfo,
		w.completeWorkflow,
		w.wth.logger,
		w.wth.enableLoggingInReplay,
		w.wth.metricsScope,
		w.wth.hostEnv)
}

func resetHistory(task *s.PollForDecisionTaskResponse, historyIterator HistoryIterator) (*s.History, error) {
	historyIterator.Reset()
	firstPageHistory, err := historyIterator.GetNextPage()
	if err != nil {
		return nil, err
	}
	task.History = firstPageHistory
	return firstPageHistory, nil
}

func (wth *workflowTaskHandlerImpl) createWorkflowContext(task *s.PollForDecisionTaskResponse) (*workflowExecutionContext, error) {
	h := task.History
	attributes := h.Events[0].WorkflowExecutionStartedEventAttributes
	if attributes == nil {
		return nil, errors.New("first history event is not WorkflowExecutionStarted")
	}
	taskList := attributes.TaskList
	if taskList == nil {
		return nil, errors.New("nil TaskList in WorkflowExecutionStarted event")
	}

	runID := task.WorkflowExecution.GetRunId()
	workflowID := task.WorkflowExecution.GetWorkflowId()

	// Setup workflow Info
	workflowInfo := &WorkflowInfo{
		WorkflowType: flowWorkflowTypeFrom(*task.WorkflowType),
		TaskListName: taskList.GetName(),
		WorkflowExecution: WorkflowExecution{
			ID:    workflowID,
			RunID: runID,
		},
		ExecutionStartToCloseTimeoutSeconds: attributes.GetExecutionStartToCloseTimeoutSeconds(),
		TaskStartToCloseTimeoutSeconds:      attributes.GetTaskStartToCloseTimeoutSeconds(),
		Domain: wth.domain,
	}
	wfStartTime := time.Unix(0, h.Events[0].GetTimestamp())
	workflowContext := &workflowExecutionContext{workflowStartTime: wfStartTime, workflowInfo: workflowInfo, wth: wth}
	workflowContext.resetWorkflowState()

	return workflowContext, nil
}

func (wth *workflowTaskHandlerImpl) getOrCreateWorkflowContext(task *s.PollForDecisionTaskResponse,
	historyIterator HistoryIterator) (workflowContext *workflowExecutionContext, skipReplayCheck bool, err error) {

	skipReplayCheck = task.Query != nil
	h := task.History
	runID := task.WorkflowExecution.GetRunId()

	workflowContext = nil
	if task.Query == nil {
		// TODO: we don't use cached workflow context for query task. Will address that shortly.
		workflowContext = getWorkflowContext(runID)
	}

	if workflowContext != nil {
		workflowContext.Lock()
		if h.Events[0].GetEventId() != workflowContext.previousStartedEventID+1 {
			// cached state is missing events, we need to discard the cached state and rebuild one.
			wth.logger.Warn("Cached sticky workflow state is missing some events.",
				zap.Int64("CachedPreviousStartedEventID", workflowContext.previousStartedEventID),
				zap.Int64("TaskFirstEventID", h.Events[0].GetEventId()),
				zap.Int64("TaskStartedEventID", task.GetStartedEventId()),
				zap.Int64("TaskPreviousStartedEventID", task.GetPreviousStartedEventId()))

			wth.metricsScope.Counter(metrics.StickyCacheStall).Inc(1)
			workflowContext.destroyCachedState()
		} else {
			// we have a valid cached state
			wth.metricsScope.Counter(metrics.StickyCacheHit).Inc(1)
			skipReplayCheck = true
		}
	} else {
		if h.Events[0].GetEventType() != s.EventTypeWorkflowExecutionStarted {
			// we are getting partial history task, but cached state was already evicted.
			// we need to reset history so we get events from beginning to replay/rebuild the state
			wth.metricsScope.Counter(metrics.StickyCacheMiss).Inc(1)
			if h, err = resetHistory(task, historyIterator); err != nil {
				return
			}
		}
		if workflowContext, err = wth.createWorkflowContext(task); err != nil {
			return
		}

		if !wth.disableStickyExecution && task.Query == nil {
			workflowContext, _ = putWorkflowContext(runID, workflowContext)
		}
		workflowContext.Lock()
	}
	if task.Query == nil {
		workflowContext.previousStartedEventID = task.GetStartedEventId()
	}

	// It is possible that 2 threads (one for decision task and one for query task) that both are getting this same
	// cached workflowContext. If one task finished with err, it would destroy the cached state. In that case, the
	// second task needs to reset the cache state and start from beginning of the history.
	if workflowContext.isDestroyed() {
		workflowContext.resetWorkflowState()
		if h, err = resetHistory(task, historyIterator); err != nil {
			return
		}
	}

	return
}

// ProcessWorkflowTask processes each all the events of the workflow task.
func (wth *workflowTaskHandlerImpl) ProcessWorkflowTask(
	task *s.PollForDecisionTaskResponse,
	historyIterator HistoryIterator,
	emitStack bool,
) (result interface{}, stackTrace string, err error) {
	if task == nil {
		return nil, "", errors.New("nil workflow task provided")
	}
	h := task.History
	if h == nil || len(h.Events) == 0 {
		return nil, "", errors.New("nil or empty history")
	}

	runID := task.WorkflowExecution.GetRunId()
	workflowID := task.WorkflowExecution.GetWorkflowId()
	traceLog(func() {
		wth.logger.Debug("Processing new workflow task.",
			zap.String(tagWorkflowType, task.WorkflowType.GetName()),
			zap.String(tagWorkflowID, workflowID),
			zap.String(tagRunID, runID),
			zap.Int64("PreviousStartedEventId", task.GetPreviousStartedEventId()))
	})

	workflowContext, skipReplayCheck, err := wth.getOrCreateWorkflowContext(task, historyIterator)
	if err != nil {
		return nil, "", err
	}
	workflowClosed := false
	defer func() {
		if err != nil || workflowClosed {
			// TODO: in case of error, ideally, we should notify server to clear the stickiness.
			// TODO: in case of closed, it asumes the close decision always succeed. need server side change to return
			// error to indicate the close failure case. This should be rear case. For now, always remove the cache, and
			// if the close decision failed, the next decision will have to rebuild the state.
			workflowContext.destroyCachedState()
			removeWorkflowContext(runID)
		}

		workflowContext.release()
	}()

	eventHandler := workflowContext.eventHandler

	reorderedHistory := newHistory(&workflowTask{task: task, historyIterator: historyIterator}, eventHandler.(*workflowExecutionEventHandlerImpl))
	decisions := []*s.Decision{}
	replayDecisions := []*s.Decision{}
	respondEvents := []*s.HistoryEvent{}

	// Process events
ProcessEvents:
	for {
		reorderedEvents, markers, err := reorderedHistory.NextDecisionEvents()
		if err != nil {
			return nil, "", err
		}

		if len(reorderedEvents) == 0 {
			break ProcessEvents
		}
		// Markers are from the events that are produced from the current decision
		for _, m := range markers {
			_, err := eventHandler.ProcessEvent(m, true, false)
			if err != nil {
				return nil, "", err
			}
		}
		isInReplay := reorderedEvents[0].GetEventId() < reorderedHistory.LastNonReplayedID()
		for i, event := range reorderedEvents {
			isLast := !isInReplay && i == len(reorderedEvents)-1
			if isDecisionEvent(event.GetEventType()) {
				respondEvents = append(respondEvents, event)
			}

			// Any metrics.
			wth.reportAnyMetrics(event, isInReplay)

			// Any pressure points.
			err := wth.executeAnyPressurePoints(event, isInReplay)
			if err != nil {
				return nil, "", err
			}

			eventDecisions, err := eventHandler.ProcessEvent(event, isInReplay, isLast)
			if err != nil {
				return nil, "", err
			}

			if eventDecisions != nil {
				if !isInReplay {
					decisions = append(decisions, eventDecisions...)
				} else {
					replayDecisions = append(replayDecisions, eventDecisions...)
				}
			}

			if workflowContext.isWorkflowCompleted {
				// If workflow is already completed then we can break from processing
				// further decisions.
				break ProcessEvents
			}
		}
	}

	if !skipReplayCheck {
		// check if decisions from reply matches to the history events
		if err := matchReplayWithHistory(replayDecisions, respondEvents); err != nil {
			wth.logger.Error("Replay and history mismatch.", zap.Error(err))
			return nil, "", err
		}
	}

	if err != nil {
		wth.logger.Error("Unable to read workflow start attributes.", zap.Error(err))
		return nil, "", err
	}

	if panicErr, ok := workflowContext.err.(*PanicError); ok {
		// Timeout the Decision instead of failing workflow.
		// TODO: Pump this stack trace on to workflow history for debuggability by exposing decision type fail to client.
		wth.metricsScope.Counter(metrics.DecisionTaskPanicCounter).Inc(1)
		wth.logger.Error("Workflow panic.",
			zap.String("PanicError", panicErr.Error()),
			zap.String("PanicStack", panicErr.StackTrace()))

		return nil, "", workflowContext.err
	}
	closeDecision := wth.completeWorkflow(workflowContext.isWorkflowCompleted, workflowContext.result, workflowContext.err)
	if closeDecision != nil {
		decisions = append(decisions, closeDecision)

		elapsed := time.Now().Sub(workflowContext.workflowStartTime)
		wth.metricsScope.Timer(metrics.WorkflowEndToEndLatency).Record(elapsed)

		switch closeDecision.GetDecisionType() {
		case s.DecisionTypeCompleteWorkflowExecution:
			wth.metricsScope.Counter(metrics.WorkflowCompletedCounter).Inc(1)
		case s.DecisionTypeFailWorkflowExecution:
			wth.metricsScope.Counter(metrics.WorkflowFailedCounter).Inc(1)
		case s.DecisionTypeCancelWorkflowExecution:
			wth.metricsScope.Counter(metrics.WorkflowCanceledCounter).Inc(1)
		case s.DecisionTypeContinueAsNewWorkflowExecution:
			wth.metricsScope.Counter(metrics.WorkflowContinueAsNewCounter).Inc(1)
		}

		workflowClosed = true
	}

	var completeRequest interface{}
	if task.Query != nil {
		// for query task
		result, err := eventHandler.ProcessQuery(task.Query.GetQueryType(), task.Query.QueryArgs)
		queryTaskCompleteRequest := &s.RespondQueryTaskCompletedRequest{
			TaskToken: task.TaskToken,
		}
		if err != nil {
			queryTaskCompleteRequest.CompletedType = common.QueryTaskCompletedTypePtr(s.QueryTaskCompletedTypeFailed)
			queryTaskCompleteRequest.ErrorMessage = common.StringPtr(err.Error())
		} else {
			queryTaskCompleteRequest.CompletedType = common.QueryTaskCompletedTypePtr(s.QueryTaskCompletedTypeCompleted)
			queryTaskCompleteRequest.QueryResult = result
		}
		completeRequest = queryTaskCompleteRequest
	} else {
		// Fill the response.
		completeRequest = &s.RespondDecisionTaskCompletedRequest{
			TaskToken: task.TaskToken,
			Decisions: decisions,
			Identity:  common.StringPtr(wth.identity),
			// ExecutionContext:
		}
		traceLog(func() {
			var buf bytes.Buffer
			for i, d := range decisions {
				buf.WriteString(fmt.Sprintf("%v: %v\n", i, util.DecisionToString(d)))
			}
			wth.logger.Debug("new_decisions",
				zap.Int("DecisionCount", len(decisions)),
				zap.String("Decisions", buf.String()))
		})
	}

	if emitStack {
		stackTrace = eventHandler.StackTrace()
	}
	return completeRequest, stackTrace, nil
}

func isVersionMarkerDecision(d *s.Decision) bool {
	if d.GetDecisionType() == s.DecisionTypeRecordMarker &&
		d.RecordMarkerDecisionAttributes.GetMarkerName() == versionMarkerName {
		return true
	}
	return false
}

func isVersionMarkerEvent(e *s.HistoryEvent) bool {
	if e.GetEventType() == s.EventTypeMarkerRecorded &&
		e.MarkerRecordedEventAttributes.GetMarkerName() == versionMarkerName {
		return true
	}
	return false
}

func matchReplayWithHistory(replayDecisions []*s.Decision, historyEvents []*s.HistoryEvent) error {
	di := 0
	hi := 0
	hSize := len(historyEvents)
	dSize := len(replayDecisions)
matchLoop:
	for hi < hSize || di < dSize {
		var e *s.HistoryEvent
		if hi < hSize {
			e = historyEvents[hi]
		}
		if isVersionMarkerEvent(e) {
			hi++
			continue matchLoop
		}

		var d *s.Decision
		if di < dSize {
			d = replayDecisions[di]
		}
		if isVersionMarkerDecision(d) {
			di++
			continue matchLoop
		}
		if d == nil {
			return fmt.Errorf("nondeterministic workflow: missing replay decision for %s", util.HistoryEventToString(e))
		}

		if e == nil {
			return fmt.Errorf("nondeterministic workflow: extra replay decision for %s", util.DecisionToString(d))
		}

		if !isDecisionMatchEvent(d, e, false) {
			return fmt.Errorf("nondeterministic workflow: history event is %s, replay decision is %s",
				util.HistoryEventToString(e), util.DecisionToString(d))
		}

		di++
		hi++
	}
	return nil
}

func isDecisionMatchEvent(d *s.Decision, e *s.HistoryEvent, strictMode bool) bool {
	switch d.GetDecisionType() {
	case s.DecisionTypeScheduleActivityTask:
		if e.GetEventType() != s.EventTypeActivityTaskScheduled {
			return false
		}
		eventAttributes := e.ActivityTaskScheduledEventAttributes
		decisionAttributes := d.ScheduleActivityTaskDecisionAttributes

		if eventAttributes.GetActivityId() != decisionAttributes.GetActivityId() ||
			eventAttributes.ActivityType.GetName() != decisionAttributes.ActivityType.GetName() ||
			(strictMode && eventAttributes.TaskList.GetName() != decisionAttributes.TaskList.GetName()) ||
			(strictMode && bytes.Compare(eventAttributes.Input, decisionAttributes.Input) != 0) {
			return false
		}

		return true

	case s.DecisionTypeRequestCancelActivityTask:
		if e.GetEventType() != s.EventTypeActivityTaskCancelRequested {
			return false
		}
		eventAttributes := e.ActivityTaskCancelRequestedEventAttributes
		decisionAttributes := d.RequestCancelActivityTaskDecisionAttributes

		if eventAttributes.GetActivityId() != decisionAttributes.GetActivityId() {
			return false
		}

		return true

	case s.DecisionTypeStartTimer:
		if e.GetEventType() != s.EventTypeTimerStarted {
			return false
		}
		eventAttributes := e.TimerStartedEventAttributes
		decisionAttributes := d.StartTimerDecisionAttributes

		if eventAttributes.GetTimerId() != decisionAttributes.GetTimerId() ||
			eventAttributes.GetStartToFireTimeoutSeconds() != decisionAttributes.GetStartToFireTimeoutSeconds() {
			return false
		}

		return true

	case s.DecisionTypeCancelTimer:
		if e.GetEventType() != s.EventTypeTimerCanceled {
			return false
		}
		eventAttributes := e.TimerCanceledEventAttributes
		decisionAttributes := d.CancelTimerDecisionAttributes

		if eventAttributes.GetTimerId() != decisionAttributes.GetTimerId() {
			return false
		}

		return true

	case s.DecisionTypeCompleteWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionCompleted {
			return false
		}
		if strictMode {
			eventAttributes := e.WorkflowExecutionCompletedEventAttributes
			decisionAttributes := d.CompleteWorkflowExecutionDecisionAttributes

			if bytes.Compare(eventAttributes.Result, decisionAttributes.Result) != 0 {
				return false
			}
		}

		return true

	case s.DecisionTypeFailWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionFailed {
			return false
		}
		if strictMode {
			eventAttributes := e.WorkflowExecutionFailedEventAttributes
			decisionAttributes := d.FailWorkflowExecutionDecisionAttributes

			if eventAttributes.GetReason() != decisionAttributes.GetReason() ||
				bytes.Compare(eventAttributes.Details, decisionAttributes.Details) != 0 {
				return false
			}
		}

		return true

	case s.DecisionTypeRecordMarker:
		if e.GetEventType() != s.EventTypeMarkerRecorded {
			return false
		}
		eventAttributes := e.MarkerRecordedEventAttributes
		decisionAttributes := d.RecordMarkerDecisionAttributes
		if eventAttributes.GetMarkerName() != decisionAttributes.GetMarkerName() {
			return false
		}

		return true

	case s.DecisionTypeRequestCancelExternalWorkflowExecution:
		if e.GetEventType() != s.EventTypeRequestCancelExternalWorkflowExecutionInitiated {
			return false
		}
		eventAttributes := e.RequestCancelExternalWorkflowExecutionInitiatedEventAttributes
		decisionAttributes := d.RequestCancelExternalWorkflowExecutionDecisionAttributes
		if eventAttributes.GetDomain() != decisionAttributes.GetDomain() ||
			eventAttributes.WorkflowExecution.GetWorkflowId() != decisionAttributes.GetWorkflowId() ||
			eventAttributes.WorkflowExecution.GetRunId() != decisionAttributes.GetRunId() {
			return false
		}

		return true

	case s.DecisionTypeCancelWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionCanceled {
			return false
		}
		if strictMode {
			eventAttributes := e.WorkflowExecutionCanceledEventAttributes
			decisionAttributes := d.CancelWorkflowExecutionDecisionAttributes
			if bytes.Compare(eventAttributes.Details, decisionAttributes.Details) != 0 {
				return false
			}
		}
		return true

	case s.DecisionTypeContinueAsNewWorkflowExecution:
		if e.GetEventType() != s.EventTypeWorkflowExecutionContinuedAsNew {
			return false
		}

		return true

	case s.DecisionTypeStartChildWorkflowExecution:
		if e.GetEventType() != s.EventTypeStartChildWorkflowExecutionInitiated {
			return false
		}
		eventAttributes := e.StartChildWorkflowExecutionInitiatedEventAttributes
		decisionAttributes := d.StartChildWorkflowExecutionDecisionAttributes
		if eventAttributes.GetDomain() != decisionAttributes.GetDomain() ||
			eventAttributes.TaskList.GetName() != decisionAttributes.TaskList.GetName() ||
			eventAttributes.WorkflowType.GetName() != decisionAttributes.WorkflowType.GetName() {
			return false
		}

		return true
	}

	return false
}

func (wth *workflowTaskHandlerImpl) completeWorkflow(
	isWorkflowCompleted bool,
	completionResult []byte,
	err error,
) *s.Decision {
	var decision *s.Decision
	if canceledErr, ok := err.(*CanceledError); ok {
		// Workflow cancelled
		decision = createNewDecision(s.DecisionTypeCancelWorkflowExecution)
		decision.CancelWorkflowExecutionDecisionAttributes = &s.CancelWorkflowExecutionDecisionAttributes{
			Details: canceledErr.details,
		}
	} else if contErr, ok := err.(*ContinueAsNewError); ok {
		// Continue as new error.
		decision = createNewDecision(s.DecisionTypeContinueAsNewWorkflowExecution)
		decision.ContinueAsNewWorkflowExecutionDecisionAttributes = &s.ContinueAsNewWorkflowExecutionDecisionAttributes{
			WorkflowType: workflowTypePtr(*contErr.options.workflowType),
			Input:        contErr.options.input,
			TaskList:     common.TaskListPtr(s.TaskList{Name: contErr.options.taskListName}),
			ExecutionStartToCloseTimeoutSeconds: contErr.options.executionStartToCloseTimeoutSeconds,
			TaskStartToCloseTimeoutSeconds:      contErr.options.taskStartToCloseTimeoutSeconds,
		}
	} else if err != nil {
		// Workflow failures
		decision = createNewDecision(s.DecisionTypeFailWorkflowExecution)
		reason, details := getErrorDetails(err)
		decision.FailWorkflowExecutionDecisionAttributes = &s.FailWorkflowExecutionDecisionAttributes{
			Reason:  common.StringPtr(reason),
			Details: details,
		}
	} else if isWorkflowCompleted {
		// Workflow completion
		decision = createNewDecision(s.DecisionTypeCompleteWorkflowExecution)
		decision.CompleteWorkflowExecutionDecisionAttributes = &s.CompleteWorkflowExecutionDecisionAttributes{
			Result: completionResult,
		}
	}
	return decision
}

func (wth *workflowTaskHandlerImpl) executeAnyPressurePoints(event *s.HistoryEvent, isInReplay bool) error {
	if wth.ppMgr != nil && !reflect.ValueOf(wth.ppMgr).IsNil() && !isInReplay {
		switch event.GetEventType() {
		case s.EventTypeDecisionTaskStarted:
			return wth.ppMgr.Execute(pressurePointTypeDecisionTaskStartTimeout)
		case s.EventTypeActivityTaskScheduled:
			return wth.ppMgr.Execute(pressurePointTypeActivityTaskScheduleTimeout)
		case s.EventTypeActivityTaskStarted:
			return wth.ppMgr.Execute(pressurePointTypeActivityTaskStartTimeout)
		case s.EventTypeDecisionTaskCompleted:
			return wth.ppMgr.Execute(pressurePointTypeDecisionTaskCompleted)
		}
	}
	return nil
}

func (wth *workflowTaskHandlerImpl) reportAnyMetrics(event *s.HistoryEvent, isInReplay bool) {
	if wth.metricsScope != nil && !isInReplay {
		switch event.GetEventType() {
		case s.EventTypeDecisionTaskTimedOut:
			wth.metricsScope.Counter(metrics.DecisionTimeoutCounter).Inc(1)
		}
	}
}

func newActivityTaskHandler(
	service workflowserviceclient.Interface,
	params workerExecutionParameters,
	env *hostEnvImpl,
) ActivityTaskHandler {
	return newActivityTaskHandlerWithCustomProvider(service, params, env, nil)
}

func newActivityTaskHandlerWithCustomProvider(
	service workflowserviceclient.Interface,
	params workerExecutionParameters,
	env *hostEnvImpl,
	activityProvider activityProvider,
) ActivityTaskHandler {
	return &activityTaskHandlerImpl{
		taskListName:     params.TaskList,
		identity:         params.Identity,
		service:          service,
		logger:           params.Logger,
		metricsScope:     params.MetricsScope,
		userContext:      params.UserContext,
		hostEnv:          env,
		activityProvider: activityProvider,
	}
}

type cadenceInvoker struct {
	sync.Mutex
	identity              string
	service               workflowserviceclient.Interface
	taskToken             []byte
	cancelHandler         func()
	retryPolicy           backoff.RetryPolicy
	heartBeatTimeoutInSec int32       // The heart beat interval configured for this activity.
	hbBatchEndTimer       *time.Timer // Whether we started a batch of operations that need to be reported in the cycle. This gets started on a user call.
	lastDetailsToReport   *[]byte
	closeCh               chan struct{}
}

func (i *cadenceInvoker) Heartbeat(details []byte) error {
	i.Lock()
	defer i.Unlock()

	if i.hbBatchEndTimer != nil {
		// If we have started batching window, keep track of last reported progress.
		i.lastDetailsToReport = &details
		return nil
	}

	isActivityCancelled, err := i.internalHeartBeat(details)

	// If the activity is cancelled, the activity can ignore the cancellation and do its work
	// and complete. Our cancellation is co-operative, so we will try to heartbeat.
	if err == nil || isActivityCancelled {
		// We have successfully sent heartbeat, start next batching window.
		i.lastDetailsToReport = nil

		// Create timer to fire before the threshold to report.
		deadlineToTrigger := i.heartBeatTimeoutInSec
		if deadlineToTrigger <= 0 {
			// If we don't have any heartbeat timeout configured.
			deadlineToTrigger = defaultHeartBeatIntervalInSec
		}

		// We set a deadline at 80% of the timeout.
		duration := time.Duration(0.8*float32(deadlineToTrigger)) * time.Second
		i.hbBatchEndTimer = time.NewTimer(duration)

		go func() {
			select {
			case <-i.hbBatchEndTimer.C:
				// We are close to deadline.
			case <-i.closeCh:
				// We got closed.
				return
			}

			// We close the batch and report the progress.
			var detailsToReport *[]byte

			i.Lock()
			detailsToReport = i.lastDetailsToReport
			i.hbBatchEndTimer.Stop()
			i.hbBatchEndTimer = nil
			i.Unlock()

			if detailsToReport != nil {
				i.Heartbeat(*detailsToReport)
			}
		}()
	}

	return err
}

func (i *cadenceInvoker) internalHeartBeat(details []byte) (bool, error) {
	isActivityCancelled := false
	err := recordActivityHeartbeat(context.Background(), i.service, i.identity, i.taskToken, details, i.retryPolicy)

	switch err.(type) {
	case *CanceledError:
		// We are asked to cancel. inform the activity about cancellation through context.
		i.cancelHandler()
		isActivityCancelled = true

	case *s.EntityNotExistsError:
		// We will pass these through as cancellation for now but something we can change
		// later when we have setter on cancel handler.
		i.cancelHandler()
		isActivityCancelled = true
	}

	// We don't want to bubble temporary errors to the user.
	// This error won't be return to user check RecordActivityHeartbeat().
	return isActivityCancelled, err
}

func (i *cadenceInvoker) Close() {
	i.Lock()
	defer i.Unlock()

	close(i.closeCh)
	if i.hbBatchEndTimer != nil {
		i.hbBatchEndTimer.Stop()
	}
}

func newServiceInvoker(
	taskToken []byte,
	identity string,
	service workflowserviceclient.Interface,
	cancelHandler func(),
	heartBeatTimeoutInSec int32,
) ServiceInvoker {
	return &cadenceInvoker{
		taskToken:             taskToken,
		identity:              identity,
		service:               service,
		cancelHandler:         cancelHandler,
		retryPolicy:           serviceOperationRetryPolicy,
		heartBeatTimeoutInSec: heartBeatTimeoutInSec,
		closeCh:               make(chan struct{}),
	}
}

// Execute executes an implementation of the activity.
func (ath *activityTaskHandlerImpl) Execute(t *s.PollForActivityTaskResponse) (result interface{}, err error) {
	traceLog(func() {
		ath.logger.Debug("Processing new activity task",
			zap.String(tagWorkflowID, t.WorkflowExecution.GetWorkflowId()),
			zap.String(tagRunID, t.WorkflowExecution.GetRunId()),
			zap.String(tagActivityType, t.ActivityType.GetName()))
	})

	rootCtx := ath.userContext
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	canCtx, cancel := context.WithCancel(rootCtx)
	invoker := newServiceInvoker(t.TaskToken, ath.identity, ath.service, cancel, t.GetHeartbeatTimeoutSeconds())
	defer invoker.Close()
	ctx := WithActivityTask(canCtx, t, invoker, ath.logger, ath.metricsScope)
	activityType := *t.ActivityType
	activityImplementation := ath.getActivity(activityType.GetName())
	if activityImplementation == nil {
		// Couldn't find the activity implementation.
		return nil, fmt.Errorf("unable to find activityType=%v", activityType.GetName())
	}

	// panic handler
	defer func() {
		if p := recover(); p != nil {
			topLine := fmt.Sprintf("activity for %s [panic]:", ath.taskListName)
			st := getStackTraceRaw(topLine, 7, 0)
			ath.logger.Error("Activity panic.",
				zap.String("PanicError", fmt.Sprintf("%v", p)),
				zap.String("PanicStack", st))
			ath.metricsScope.Counter(metrics.ActivityTaskPanicCounter).Inc(1)
			panicErr := newPanicError(p, st)
			result, err = convertActivityResultToRespondRequest(ath.identity, t.TaskToken, nil, panicErr), nil
		}
	}()

	var deadline time.Time
	scheduleToCloseDeadline := time.Unix(0, t.GetScheduledTimestamp()).Add(time.Duration(t.GetScheduleToCloseTimeoutSeconds()) * time.Second)
	startToCloseDeadline := time.Unix(0, t.GetStartedTimestamp()).Add(time.Duration(t.GetStartToCloseTimeoutSeconds()) * time.Second)
	// Minimum of the two deadlines.
	if scheduleToCloseDeadline.Before(startToCloseDeadline) {
		deadline = scheduleToCloseDeadline
	} else {
		deadline = startToCloseDeadline
	}
	ctx, dlCancelFunc := context.WithDeadline(ctx, deadline)

	output, err := activityImplementation.Execute(ctx, t.Input)

	dlCancelFunc()
	if <-ctx.Done(); ctx.Err() == context.DeadlineExceeded {
		return nil, ctx.Err()
	}

	return convertActivityResultToRespondRequest(ath.identity, t.TaskToken, output, err), nil
}

func (ath *activityTaskHandlerImpl) getActivity(name string) activity {
	if ath.activityProvider != nil {
		return ath.activityProvider(name)
	}

	if a, ok := ath.hostEnv.getActivity(name); ok {
		return a
	}

	return nil
}

func createNewDecision(decisionType s.DecisionType) *s.Decision {
	return &s.Decision{
		DecisionType: common.DecisionTypePtr(decisionType),
	}
}

func recordActivityHeartbeat(
	ctx context.Context,
	service workflowserviceclient.Interface,
	identity string,
	taskToken, details []byte,
	retryPolicy backoff.RetryPolicy,
) error {
	request := &s.RecordActivityTaskHeartbeatRequest{
		TaskToken: taskToken,
		Details:   details,
		Identity:  common.StringPtr(identity)}

	var heartbeatResponse *s.RecordActivityTaskHeartbeatResponse
	heartbeatErr := backoff.Retry(ctx,
		func() error {
			tchCtx, cancel, opt := newChannelContext(ctx)
			defer cancel()

			var err error
			heartbeatResponse, err = service.RecordActivityTaskHeartbeat(tchCtx, request, opt...)
			return err
		}, retryPolicy, isServiceTransientError)

	if heartbeatErr == nil && heartbeatResponse != nil && heartbeatResponse.GetCancelRequested() {
		return NewCanceledError()
	}

	return heartbeatErr
}

// This enables verbose logging in the client library.
// check Cadence.EnableVerboseLogging()
func traceLog(fn func()) {
	if enableVerboseLogging {
		fn()
	}
}
