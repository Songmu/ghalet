package ghalet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

type fakeSession struct {
	mu         sync.Mutex
	messages   []*scaleset.RunnerScaleSetMessage
	acquired   []int64
	deleted    []int
	capacities []int
	acquireErr error
	getErr     error
	cancel     context.CancelFunc
}

func (f *fakeSession) GetMessage(ctx context.Context, _, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacities = append(f.capacities, capacity)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.messages) == 0 {
		if f.cancel != nil {
			f.cancel()
		}
		return nil, ctx.Err()
	}
	message := f.messages[0]
	f.messages = f.messages[1:]
	return message, nil
}

func (f *fakeSession) DeleteMessage(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, ids...)
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	return ids, nil
}

func (f *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: [16]byte(uuid.New()), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

func availableMessage(id, count int) *scaleset.RunnerScaleSetMessage {
	message := &scaleset.RunnerScaleSetMessage{MessageID: id, Statistics: &scaleset.RunnerScaleSetStatistic{}}
	for i := 0; i < count; i++ {
		message.JobAvailableMessages = append(message.JobAvailableMessages, &scaleset.JobAvailable{
			JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: int64(id*100 + i)},
		})
	}
	return message
}

func TestAcquisitionReservesSharedCapacityAndReplayIsIdempotent(t *testing.T) {
	d, _ := testDaemon(t)
	first := d.targets["build"]
	second := *first
	second.Config.ID = "second"
	d.targets["second"] = &second
	session := &fakeSession{}
	a := &targetScaler{daemon: d, target: first, session: session}
	b := &targetScaler{daemon: d, target: &second, session: session}
	message := availableMessage(1, 5)
	if err := a.Scale(t.Context(), message); err != nil || len(session.acquired) != 2 || first.Pending != 2 {
		t.Fatal("acquisition exceeded or did not reserve shared capacity", err)
	}
	if err := a.Scale(t.Context(), message); err != nil || len(session.acquired) != 2 {
		t.Fatal("redelivery duplicated external acquisition")
	}
	if err := b.Scale(t.Context(), availableMessage(2, 5)); err != nil || len(session.acquired) != 2 {
		t.Fatal("another target oversubscribed pending capacity")
	}
	if first.Assigned != 0 {
		t.Fatal("individual messages were used as authoritative scaling demand")
	}
	if err := a.Scale(t.Context(), &scaleset.RunnerScaleSetMessage{MessageID: 3,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}); err != nil || first.Assigned != 2 || first.Pending != 0 {
		t.Fatal("authoritative statistics did not replace pending reservation", err)
	}
}

func TestAcquisitionAccumulatesReservationsWithoutStatistics(t *testing.T) {
	for _, failure := range []int{0, 1, 2} {
		t.Run(map[int]string{0: "success", 1: "first ambiguous", 2: "second ambiguous"}[failure], func(t *testing.T) {
			d, _ := testDaemon(t)
			target := d.targets["build"]
			session := &fakeSession{}
			scaler := &targetScaler{daemon: d, target: target, session: session}
			for id := 1; id <= 3; id++ {
				session.acquireErr = nil
				if id == failure {
					session.acquireErr = errTestAPI
				}
				message := availableMessage(id, 1)
				message.Statistics = nil
				err := scaler.Scale(t.Context(), message)
				if (err != nil) != (id == failure) {
					t.Fatal("unexpected acquisition result", err)
				}
				if target.Pending != min(id, 2) || len(session.acquired) != min(id, 2) {
					t.Fatal("successive messages replaced reservations or exceeded capacity", target.Pending, session.acquired)
				}
			}
			message := availableMessage(4, 0)
			message.Statistics.TotalAssignedJobs = 2
			if err := scaler.Scale(t.Context(), message); err != nil || target.Pending != 0 || target.Assigned != 2 {
				t.Fatal("authoritative statistics did not replace accumulated reservations", err)
			}
		})
	}
}

func TestDrainRejectsNewAcquisitionsButKeepsAuthoritativeAssignedWork(t *testing.T) {
	d, _ := testDaemon(t)
	target := d.targets["build"]
	d.beginDrain(target)
	session := &fakeSession{}
	scaler := &targetScaler{daemon: d, target: target, session: session}
	message := availableMessage(1, 5)
	message.Statistics.TotalAssignedJobs = 1
	if err := scaler.Scale(t.Context(), message); err != nil || len(session.acquired) != 0 || target.Assigned != 1 {
		t.Fatal("drain acquired new jobs or discarded previously accepted work", err)
	}
}

func TestOfficialListenerAcknowledgesOnlySuccessfulCallbacks(t *testing.T) {
	for _, fail := range []bool{false, true} {
		d, _ := testDaemon(t)
		ctx, cancel := context.WithCancel(t.Context())
		session := &fakeSession{messages: []*scaleset.RunnerScaleSetMessage{availableMessage(7, 1)}, cancel: cancel}
		if fail {
			session.acquireErr = errTestAPI
		}
		target := d.targets["build"]
		l, err := listener.New(session, listener.Config{ScaleSetID: 10, MaxRunners: 2})
		if err != nil {
			t.Fatal(err)
		}
		err = l.Run(ctx, &targetScaler{daemon: d, target: target, session: session})
		cancel()
		if fail {
			if err == nil || len(session.deleted) != 0 || target.Pending != 1 {
				t.Fatal("failed acquisition was acked or lost its conservative reservation")
			}
		} else if !errors.Is(err, context.Canceled) || len(session.deleted) != 1 || session.deleted[0] != 7 {
			t.Fatal("successful callback was not acknowledged", err)
		}
		if len(session.capacities) == 0 || session.capacities[0] != 2 {
			t.Fatal("capacity notification prevented demand observation while warm slots are occupied")
		}
	}
}

func TestBusyAndCompletedEventsMatchExactRunner(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.RunnerID = 123
	d.runs[state.RunID] = &activeRun{State: state}
	d.store.save(state)
	target := d.targets["build"]
	scaler := &targetScaler{daemon: d, target: target, session: &fakeSession{}}
	message := &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobStartedMessages: []*scaleset.JobStarted{{RunnerID: 123, RunnerName: state.RunnerName}}}
	if err := scaler.Scale(t.Context(), message); err != nil || !state.Busy || !state.Ready {
		t.Fatal("job started was not persisted", err)
	}
	message.MessageID = 2
	message.JobStartedMessages = nil
	message.JobCompletedMessages = []*scaleset.JobCompleted{{RunnerID: 123, RunnerName: state.RunnerName}}
	if err := scaler.Scale(t.Context(), message); err != nil || state.Busy || state.Phase != "exiting" || state.ProcessDone {
		t.Fatal("completion confused workflow status with process exit", err)
	}
}
