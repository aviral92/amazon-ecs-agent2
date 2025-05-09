// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package eventhandler

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/api"
	"github.com/aws/amazon-ecs-agent/agent/data"
	"github.com/aws/amazon-ecs-agent/agent/engine/dockerstate"
	"github.com/aws/amazon-ecs-agent/agent/statechange"
	"github.com/aws/amazon-ecs-agent/agent/utils"
	"github.com/aws/amazon-ecs-agent/ecs-agent/api/ecs"
	apierrors "github.com/aws/amazon-ecs-agent/ecs-agent/api/errors"
	apitaskstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/task/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils/retry"
	"github.com/cihub/seelog"
)

const (
	// concurrentEventCalls is the maximum number of tasks that may be handled at
	// once by the TaskHandler
	concurrentEventCalls = 10

	// drainEventsFrequency is the frequency at the which unsent events batched
	// by the task handler are sent to the backend
	minDrainEventsFrequency = 10 * time.Second
	maxDrainEventsFrequency = 30 * time.Second

	submitStateBackoffMin            = time.Second
	submitStateBackoffMax            = 30 * time.Second
	submitStateBackoffJitterMultiple = 0.20
	submitStateBackoffMultiple       = 1.3
)

// TaskHandler encapsulates the the map of a task arn to task and container events
// associated with said task
type TaskHandler struct {
	// submitSemaphore for the number of tasks that may be handled at once
	submitSemaphore utils.Semaphore
	// taskToEvents is arn:*eventList map so events may be serialized per task
	tasksToEvents map[string]*taskSendableEvents
	// tasksToContainerStates is used to collect container events
	// between task transitions
	tasksToContainerStates map[string][]api.ContainerStateChange
	// tasksToManagedAgentStates is used to collect managed agent events
	tasksToManagedAgentStates map[string][]api.ManagedAgentStateChange
	//  taskHandlerLock is used to safely access the following maps:
	// * taskToEvents
	// * tasksToContainerStates
	lock sync.RWMutex

	// dataClient is used to save changes to database, mainly to save
	// changes of a task or container's SentStatus.
	dataClient data.Client

	// min and max drain events frequency refer to the range of
	// time over which a call to SubmitTaskStateChange is made.
	// The actual duration is randomly distributed between these
	// two
	minDrainEventsFrequency time.Duration
	maxDrainEventsFrequency time.Duration

	state  dockerstate.TaskEngineState
	client ecs.ECSClient
	ctx    context.Context
}

// taskSendableEvents is used to group all events for a task
type taskSendableEvents struct {
	// events is a list of *sendableEvents. We treat this as queue, where
	// new events are added to the back of the queue and old events are
	// drained from the front. `sendChange` pushes an event to the back of
	// the queue. An event is removed from the queue in `submitFirstEvent`
	events *list.List
	// sending will check whether the list is already being handled
	sending bool
	//eventsListLock locks both the list and sending bool
	lock sync.Mutex
	// createdAt is a timestamp for when the event list was created
	createdAt time.Time
	// taskARN is the task arn that the event list is associated with
	taskARN string
}

// NewTaskHandler returns a pointer to TaskHandler
func NewTaskHandler(ctx context.Context,
	dataClient data.Client,
	state dockerstate.TaskEngineState,
	client ecs.ECSClient) *TaskHandler {
	// Create a handler and start the periodic event drain loop
	taskHandler := &TaskHandler{
		ctx:                       ctx,
		tasksToEvents:             make(map[string]*taskSendableEvents),
		submitSemaphore:           utils.NewSemaphore(concurrentEventCalls),
		tasksToContainerStates:    make(map[string][]api.ContainerStateChange),
		tasksToManagedAgentStates: make(map[string][]api.ManagedAgentStateChange),
		dataClient:                dataClient,
		state:                     state,
		client:                    client,
		minDrainEventsFrequency:   minDrainEventsFrequency,
		maxDrainEventsFrequency:   maxDrainEventsFrequency,
	}
	go taskHandler.startDrainEventsTicker()

	return taskHandler
}

// AddStateChangeEvent queues up the state change event to be sent to ECS.
// If the event is for a container state change, it just gets added to the
// handler.tasksToContainerStates map.
// If the event is for a managed agent state change, it just gets added to the
// handler.tasksToManagedAgentStates map.
// If the event is for task state change, it triggers the non-blocking
// handler.submitTaskEvents method to submit the batched container state
// changes and the task state change to ECS
func (handler *TaskHandler) AddStateChangeEvent(change statechange.Event, client ecs.ECSClient) error {
	handler.lock.Lock()
	defer handler.lock.Unlock()
	switch change.GetEventType() {
	case statechange.TaskEvent:
		event, ok := change.(api.TaskStateChange)
		if !ok {
			return errors.New("eventhandler: unable to get task event from state change event")
		}
		// Task event: gather all the container and managed agent events and send them
		// to ECS by invoking the async submitTaskEvents method from
		// the sendable event list object
		handler.flushBatchUnsafe(&event, client)
		return nil

	case statechange.ContainerEvent:
		event, ok := change.(api.ContainerStateChange)
		if !ok {
			return errors.New("eventhandler: unable to get container event from state change event")
		}
		handler.batchContainerEventUnsafe(event)
		return nil

	case statechange.ManagedAgentEvent:
		event, ok := change.(api.ManagedAgentStateChange)
		if !ok {
			return errors.New("eventhandler: unable to get managed agent event from state change event")
		}

		handler.batchManagedAgentEventUnsafe(event)
		return nil

	default:
		return errors.New("eventhandler: unable to determine event type from state change event")
	}
}

// startDrainEventsTicker starts a ticker that periodically drains the events queue
// by submitting state change events to the ECS backend
func (handler *TaskHandler) startDrainEventsTicker() {
	derivedCtx, cancel := context.WithCancel(handler.ctx)
	defer cancel()
	ticker := utils.NewJitteredTicker(derivedCtx, handler.minDrainEventsFrequency, handler.maxDrainEventsFrequency)
	for {
		select {
		case <-handler.ctx.Done():
			seelog.Infof("TaskHandler: Stopping periodic container/managed agent state change submission ticker")
			return
		case <-ticker:
			// Gather a list of task state changes to send. This list is constructed from
			// the tasksToContainerStates and tasksToManagedAgentStates maps based on the
			// task arns of containers and managed agents that haven't been sent to ECS yet.
			for _, taskEvent := range handler.taskStateChangesToSend() {
				logger.Debug("TaskHandler: Adding a state change event to send batched container/managed agent events",
					taskEvent.ToFields())
				// Force start the the task state change submission
				// workflow by calling AddStateChangeEvent method.
				handler.AddStateChangeEvent(taskEvent, handler.client)
			}
		}
	}
}

// taskStateChangesToSend gets a list task state changes for container events that
// have been batched and not sent beyond the drainEventsFrequency threshold
func (handler *TaskHandler) taskStateChangesToSend() []api.TaskStateChange {
	handler.lock.RLock()
	defer handler.lock.RUnlock()

	events := make(map[string]api.TaskStateChange)
	for taskARN := range handler.tasksToContainerStates {
		// An entry for the task in tasksToContainerStates means that there
		// is at least 1 container event for that task that hasn't been sent
		// to ECS (has been batched).
		// Make sure that the engine's task state knows about this task (as a
		// safety mechanism) and add it to the list of task state changes
		// that need to be sent to ECS
		if task, ok := handler.state.TaskByArn(taskARN); ok {
			// We do not allow the ticker to submit container state updates for
			// tasks that are STOPPED. This prevents the ticker's asynchronous
			// updates from clobbering container states when the task
			// transitions to STOPPED, since ECS does not allow updates to
			// container states once the task has moved to STOPPED.
			knownStatus := task.GetKnownStatus()
			if knownStatus >= apitaskstatus.TaskStopped {
				continue
			}
			event := api.TaskStateChange{
				TaskARN: taskARN,
				Status:  task.GetKnownStatus(),
				Task:    task,
			}
			event.SetTaskTimestamps()
			events[taskARN] = event
		}
	}

	for taskARN := range handler.tasksToManagedAgentStates {
		if _, ok := events[taskARN]; ok {
			continue
		}
		if task, ok := handler.state.TaskByArn(taskARN); ok {
			// We do not allow the ticker to submit managed agent state updates for
			// tasks that are STOPPED. This prevents the ticker's asynchronous
			// updates from clobbering managed agent states when the task
			// transitions to STOPPED, since ECS does not allow updates to
			// managed agent states once the task has moved to STOPPED.
			knownStatus := task.GetKnownStatus()
			if knownStatus >= apitaskstatus.TaskStopped {
				continue
			}
			event := api.TaskStateChange{
				TaskARN: taskARN,
				Status:  task.GetKnownStatus(),
				Task:    task,
			}
			event.SetTaskTimestamps()

			events[taskARN] = event
		}
	}
	var taskEvents []api.TaskStateChange
	for _, tEvent := range events {
		taskEvents = append(taskEvents, tEvent)
	}
	return taskEvents
}

// batchContainerEventUnsafe collects container state change events for a given task arn
func (handler *TaskHandler) batchContainerEventUnsafe(event api.ContainerStateChange) {
	seelog.Debugf("TaskHandler: batching container event: %s", event.String())
	handler.tasksToContainerStates[event.TaskArn] = append(handler.tasksToContainerStates[event.TaskArn], event)
}

// batchManagedAgentEventUnsafe collects managed agent state change events for a given task arn
func (handler *TaskHandler) batchManagedAgentEventUnsafe(event api.ManagedAgentStateChange) {
	seelog.Debugf("TaskHandler: batching managed agent event: %s", event.String())
	handler.tasksToManagedAgentStates[event.TaskArn] = append(handler.tasksToManagedAgentStates[event.TaskArn], event)
}

// flushBatchUnsafe attaches the task arn's container events to TaskStateChange event
// by creating the sendable event list. It then submits this event to ECS asynchronously
func (handler *TaskHandler) flushBatchUnsafe(taskStateChange *api.TaskStateChange, client ecs.ECSClient) {
	taskStateChange.Containers = append(taskStateChange.Containers,
		handler.tasksToContainerStates[taskStateChange.TaskARN]...)
	// All container events for the task have now been copied to the
	// task state change object. Remove them from the map
	delete(handler.tasksToContainerStates, taskStateChange.TaskARN)
	taskStateChange.ManagedAgents = append(taskStateChange.ManagedAgents,
		handler.tasksToManagedAgentStates[taskStateChange.TaskARN]...)
	// All managed agent events for the task have now been copied to the
	// task state change object. Remove them from the map
	delete(handler.tasksToManagedAgentStates, taskStateChange.TaskARN)
	// Prepare a given event to be sent by adding it to the handler's
	// eventList
	event := newSendableTaskEvent(*taskStateChange)
	taskEvents := handler.getTaskEventsUnsafe(event)

	// Add the event to the sendable events queue for the task and
	// start sending it asynchronously if possible
	taskEvents.sendChange(event, client, handler)
}

// getTaskEventsUnsafe gets the event list for the task arn in the sendableEvent
// from taskToEvent map
func (handler *TaskHandler) getTaskEventsUnsafe(event *sendableEvent) *taskSendableEvents {
	taskARN := event.taskArn()
	taskEvents, ok := handler.tasksToEvents[taskARN]

	if !ok {
		// We are not tracking this task arn in the tasksToEvents map. Create
		// a new entry
		taskEvents = &taskSendableEvents{
			events:    list.New(),
			sending:   false,
			createdAt: time.Now(),
			taskARN:   taskARN,
		}
		handler.tasksToEvents[taskARN] = taskEvents
		logger.Debug(fmt.Sprintf("TaskHandler: collecting events for new task; events: %s", taskEvents.toStringUnsafe()), event.toFields())
	}

	return taskEvents
}

// Continuously retries sending an event until it succeeds, sleeping between each
// attempt
func (handler *TaskHandler) submitTaskEvents(taskEvents *taskSendableEvents, client ecs.ECSClient, taskARN string) {
	defer handler.removeTaskEvents(taskARN)

	backoff := retry.NewExponentialBackoff(submitStateBackoffMin, submitStateBackoffMax,
		submitStateBackoffJitterMultiple, submitStateBackoffMultiple)

	// Mirror events.sending, but without the need to lock since this is local
	// to our goroutine
	done := false
	// TODO: wire in the context here. Else, we have go routine leaks in tests
	for !done {
		// If we looped back up here, we successfully submitted an event, but
		// we haven't emptied the list so we should keep submitting
		backoff.Reset()
		retry.RetryWithBackoff(backoff, func() error {
			// Lock and unlock within this function, allowing the list to be added
			// to while we're not actively sending an event
			seelog.Debug("TaskHandler: Waiting on semaphore to send events...")
			handler.submitSemaphore.Wait()
			defer handler.submitSemaphore.Post()

			var err error
			done, err = taskEvents.submitFirstEvent(handler, backoff)
			return err
		})
	}
}

func (handler *TaskHandler) removeTaskEvents(taskARN string) {
	handler.lock.Lock()
	defer handler.lock.Unlock()

	delete(handler.tasksToEvents, taskARN)
}

// sendChange adds the change to the sendable events queue. It triggers
// the handler's submitTaskEvents async method to submit this change if
// there's no go routines already sending changes for this event list
func (taskEvents *taskSendableEvents) sendChange(change *sendableEvent,
	client ecs.ECSClient,
	handler *TaskHandler) {

	taskEvents.lock.Lock()
	defer taskEvents.lock.Unlock()

	// Add event to the queue
	logger.Debug("TaskHandler: Adding event", change.toFields())
	taskEvents.events.PushBack(change)

	if !taskEvents.sending {
		// If a send event is not already in progress, trigger the
		// submitTaskEvents to start sending changes to ECS
		taskEvents.sending = true
		go handler.submitTaskEvents(taskEvents, client, change.taskArn())
	} else {
		logger.Debug(
			"TaskHandler: Not submitting change as the task is already being sent",
			change.toFields())
	}
}

// submitFirstEvent submits the first event for the task from the event list. It
// returns true if the list became empty after submitting the event. Else, it returns
// false. An error is returned if there was an error with submitting the state change
// to ECS. The error is used by the backoff handler to backoff before retrying the
// state change submission for the first event
func (taskEvents *taskSendableEvents) submitFirstEvent(handler *TaskHandler, backoff retry.Backoff) (bool, error) {
	seelog.Debug("TaskHandler: Acquiring lock for sending event...")
	taskEvents.lock.Lock()
	defer taskEvents.lock.Unlock()

	seelog.Debugf("TaskHandler: Acquired lock, processing event list: : %s", taskEvents.toStringUnsafe())

	if taskEvents.events.Len() == 0 {
		seelog.Debug("TaskHandler: No events left; not retrying more")
		taskEvents.sending = false
		return true, nil
	}

	eventToSubmit := taskEvents.events.Front()
	// Extract the wrapped event from the list element
	event := eventToSubmit.Value.(*sendableEvent)

	if event.containerShouldBeSent() {
		if err := event.send(sendContainerStatusToECS, setContainerChangeSent, "container",
			handler.client, eventToSubmit, handler.dataClient, backoff, taskEvents); err != nil {
			return false, err
		}
	} else if event.taskShouldBeSent() {
		if err := event.send(sendTaskStatusToECS, setTaskChangeSent, "task",
			handler.client, eventToSubmit, handler.dataClient, backoff, taskEvents); err != nil {
			handleInvalidParamException(err, taskEvents.events, eventToSubmit)
			return false, err
		}
	} else if event.taskAttachmentShouldBeSent() {
		if err := event.send(sendTaskStatusToECS, setTaskAttachmentSent, "task attachment",
			handler.client, eventToSubmit, handler.dataClient, backoff, taskEvents); err != nil {
			handleInvalidParamException(err, taskEvents.events, eventToSubmit)
			return false, err
		}
	} else {
		// Shouldn't be sent as either a task or container change event; must have been already sent
		logger.Info("TaskHandler: Not submitting redundant event; just removing", event.toFields())
		taskEvents.events.Remove(eventToSubmit)
	}

	if taskEvents.events.Len() == 0 {
		logger.Debug("TaskHandler: Removed the last element, no longer sending")
		taskEvents.sending = false
		return true, nil
	}

	return false, nil
}

func (taskEvents *taskSendableEvents) toStringUnsafe() string {
	return fmt.Sprintf("Task event list [taskARN: %s, sending: %t, createdAt: %s]",
		taskEvents.taskARN, taskEvents.sending, taskEvents.createdAt.String())
}

// handleInvalidParamException removes the event from event queue when its parameters are
// invalid to reduce redundant API call
func handleInvalidParamException(err error, events *list.List, eventToSubmit *list.Element) {
	if utils.IsAWSErrorCodeEqual(err, apierrors.ErrCodeInvalidParameterException) {
		event := eventToSubmit.Value.(*sendableEvent)
		logger.Warn("TaskHandler: Event is sent with invalid parameters; just removing", event.toFields())
		events.Remove(eventToSubmit)
	}
}
