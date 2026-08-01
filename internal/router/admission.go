package router

import (
	"context"
	"sync/atomic"
	"time"
)

type admissionOutcome string

const (
	admissionAcquired admissionOutcome = "acquired"
	admissionFull     admissionOutcome = "queue_full"
	admissionTimeout  admissionOutcome = "queue_timeout"
	admissionCanceled admissionOutcome = "canceled"
)

// admissionController 在读取请求体之前做进程级准入，防止慢上游把内存和 goroutine 队列拖垮。
type admissionController struct {
	slots        chan struct{}
	maxQueue     int64
	queueTimeout time.Duration
	inflight     atomic.Int64
	queued       atomic.Int64
}

func newAdmissionController(cfg BackpressureConfig) *admissionController {
	if cfg.MaxInflight <= 0 {
		return nil
	}
	return &admissionController{
		slots:        make(chan struct{}, cfg.MaxInflight),
		maxQueue:     int64(cfg.MaxQueue),
		queueTimeout: cfg.QueueTimeout,
	}
}

func (a *admissionController) acquire(ctx context.Context) (func(), admissionOutcome) {
	select {
	case a.slots <- struct{}{}:
		a.inflight.Add(1)
		return a.release, admissionAcquired
	default:
	}
	if !a.reserveQueueSlot() {
		return nil, admissionFull
	}
	defer a.queued.Add(-1)

	timer := time.NewTimer(a.queueTimeout)
	defer timer.Stop()
	select {
	case a.slots <- struct{}{}:
		a.inflight.Add(1)
		return a.release, admissionAcquired
	case <-timer.C:
		return nil, admissionTimeout
	case <-ctx.Done():
		return nil, admissionCanceled
	}
}

func (a *admissionController) reserveQueueSlot() bool {
	for {
		queued := a.queued.Load()
		if queued >= a.maxQueue {
			return false
		}
		if a.queued.CompareAndSwap(queued, queued+1) {
			return true
		}
	}
}

func (a *admissionController) release() {
	<-a.slots
	a.inflight.Add(-1)
}
