package usage

import "context"

type recordObserverContextKey struct{}

// WithRecordObserver attaches an internal synchronous observer to an execution.
// It does not register a global usage plugin or change plugin delivery.
func WithRecordObserver(ctx context.Context, observer func(context.Context, Record)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, recordObserverContextKey{}, observer)
}

// ObserveRecord updates internal request-time state before asynchronous usage delivery.
func ObserveRecord(ctx context.Context, record Record) {
	if ctx == nil {
		return
	}
	if observer, ok := ctx.Value(recordObserverContextKey{}).(func(context.Context, Record)); ok && observer != nil {
		observer(ctx, record)
	}
}
