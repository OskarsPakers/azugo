package azugo

import (
	"context"
	"runtime"
	"testing"
	"time"

	"azugo.io/core/http"
	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

func TestImplementsContextInterface(t *testing.T) {
	qt.Check(t, qt.Implements[context.Context](&Context{}))
}

type testExtValueContext struct{}

func (t *testExtValueContext) Context(ctx context.Context) context.Context {
	return context.WithValue(RequestContext(ctx).Context(), "test", "value")
}

type testExtDeadlineContext struct {
	cancel context.CancelFunc
}

func (t *testExtDeadlineContext) Context(ctx context.Context) context.Context {
	if t.cancel != nil {
		t.cancel()
	}

	c, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	t.cancel = cancel

	return c
}

// TestContextValueExtension covers the deprecated ExtendedContext hook path.
func TestContextValueExtension(t *testing.T) {
	app := NewTestApp()

	app.SetExtendedContext(&testExtValueContext{})

	app.Start(t)
	defer app.Stop()

	app.Get("/test", func(ctx *Context) {
		qt.Check(t, qt.IsNil(ctx.Value("missing")))

		v := ctx.Value("test")

		if v == "value" {
			ctx.StatusCode(200)
			return
		}

		ctx.StatusCode(500)
	})

	resp, err := app.TestClient().Get("/test")
	qt.Assert(t, qt.IsNil(err))

	qt.Check(t, qt.Equals(resp.StatusCode(), http.StatusOK))
}

type testTxKeyType struct{}

var testTxKey testTxKeyType

type testTxContext struct {
	context.Context

	parent context.Context
}

func (c *testTxContext) RequestContext() context.Context { return c.parent }

func wrapTestTx(ctx context.Context) *testTxContext {
	t := &testTxContext{parent: ctx}
	t.Context = context.WithValue(ctx, testTxKey, t)

	return t
}

func TestContextSetContext(t *testing.T) {
	app := NewTestApp()
	app.Start(t)
	defer app.Stop()

	type pushKey struct{}

	deadline := time.Now().Add(time.Minute)

	app.Get("/test", func(ctx *Context) {
		ctx.SetUserValue("base-key", "base-val")

		ctx.SetContext(context.WithValue(ctx.Context(), pushKey{}, "push-val"))
		qt.Check(t, qt.Equals(ctx.Value(pushKey{}).(string), "push-val"))
		qt.Check(t, qt.Equals(ctx.Value("base-key").(string), "base-val"))
		qt.Check(t, qt.IsNil(ctx.Value("missing")))

		dctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		ctx.SetContext(dctx)
		d, ok := ctx.Deadline()
		qt.Check(t, qt.IsTrue(ok))
		qt.Check(t, qt.IsTrue(d.Equal(deadline)))

		var reset context.Context
		ctx.SetContext(reset)
		qt.Check(t, qt.IsNil(ctx.Value(pushKey{})))
		qt.Check(t, qt.Equals(ctx.Value("base-key").(string), "base-val"))

		ctx.StatusCode(http.StatusNoContent)
	})

	resp, err := app.TestClient().Get("/test")
	defer fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(resp.StatusCode(), http.StatusNoContent))
}

func TestRequestContextRecovery(t *testing.T) {
	app := NewTestApp()
	app.Start(t)
	defer app.Stop()

	type slowKey struct{}

	app.Get("/test", func(ctx *Context) {
		qt.Check(t, qt.Equals(RequestContext(ctx), ctx))
		tx := wrapTestTx(ctx)
		qt.Check(t, qt.Equals(RequestContext(tx), ctx))
		qt.Check(t, qt.Equals(RequestContext(context.WithValue(tx, slowKey{}, "5s")), ctx))
		qt.Check(t, qt.Equals(RequestContext(context.WithValue(ctx, slowKey{}, "x")), ctx))
		qt.Check(t, qt.IsNil(RequestContext(context.Background())))
		var noContext context.Context
		qt.Check(t, qt.IsNil(RequestContext(noContext)))

		ctx.StatusCode(http.StatusNoContent)
	})

	resp, err := app.TestClient().Get("/test")
	defer fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(resp.StatusCode(), http.StatusNoContent))
}

func TestContextTransactionAndSpanStack(t *testing.T) {
	app := NewTestApp()
	app.Start(t)
	defer app.Stop()

	type spanKey struct{}

	type slowKey struct{}

	app.Get("/test", func(ctx *Context) {
		ctx.SetContext(context.WithValue(ctx.Context(), spanKey{}, "span"))

		tx := wrapTestTx(ctx)
		stack := context.WithValue(tx, slowKey{}, "5s")

		qt.Check(t, qt.Equals(RequestContext(stack), ctx))
		qt.Check(t, qt.Equals(stack.Value(spanKey{}).(string), "span"))
		qt.Check(t, qt.Equals(stack.Value(testTxKey).(*testTxContext), tx))
		qt.Check(t, qt.Equals(stack.Value(slowKey{}).(string), "5s"))

		ctx.StatusCode(http.StatusNoContent)
	})

	resp, err := app.TestClient().Get("/test")
	defer fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(resp.StatusCode(), http.StatusNoContent))
}

func TestContextDeadlineExtension(t *testing.T) {
	app := NewTestApp()

	ext := &testExtDeadlineContext{}
	app.SetExtendedContext(ext)

	app.Start(t)
	defer app.Stop()
	t.Cleanup(func() {
		if ext.cancel != nil {
			ext.cancel()
		}
	})

	app.Get("/test", func(ctx *Context) {
		_, ok := ctx.Deadline()

		if ok {
			ctx.StatusCode(200)
			return
		}

		ctx.StatusCode(500)
	})

	resp, err := app.TestClient().Get("/test")
	qt.Assert(t, qt.IsNil(err))

	qt.Check(t, qt.Equals(resp.StatusCode(), http.StatusOK))
}

// Deriving from the Context must take the stdlib's goroutine-free AfterFunc
// path instead of starting a watcher goroutine.
var _ interface{ AfterFunc(func()) func() bool } = (*Context)(nil)

// A cancellable child derived from the request Context is canceled when the
// request ends, even if the handler never calls cancel itself.
func TestDerivedContextCanceledAtRequestEnd(t *testing.T) {
	app := NewTestApp()

	app.Start(t)
	defer app.Stop()

	type derived struct {
		ctx    context.Context
		cancel context.CancelFunc
	}

	children := make(chan derived, 1)

	app.Get("/test", func(ctx *Context) {
		child, cancel := context.WithCancel(ctx)
		children <- derived{ctx: child, cancel: cancel}
	})

	resp, err := app.TestClient().Get("/test")
	qt.Assert(t, qt.IsNil(err))
	fasthttp.ReleaseResponse(resp)

	d := <-children
	defer d.cancel()

	select {
	case <-d.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("derived context was not canceled at request end")
	}

	qt.Check(t, qt.ErrorIs(d.ctx.Err(), context.Canceled))
}

// The done channel handed out by the Context stays valid after the handler
// returns and is closed at request end.
func TestContextDoneClosedAtRequestEnd(t *testing.T) {
	app := NewTestApp()

	app.Start(t)
	defer app.Stop()

	dones := make(chan (<-chan struct{}), 1)

	app.Get("/test", func(ctx *Context) {
		qt.Check(t, qt.IsNil(ctx.Err()))

		dones <- ctx.Done()
	})

	resp, err := app.TestClient().Get("/test")
	qt.Assert(t, qt.IsNil(err))
	fasthttp.ReleaseResponse(resp)

	select {
	case <-(<-dones):
	case <-time.After(time.Second):
		t.Fatal("request done channel was not closed at request end")
	}
}

// Deriving a cancellable context must not take the Context out of the pool.
func TestContextRecycledAfterDerivation(t *testing.T) {
	app := NewTestApp()

	app.Start(t)
	defer app.Stop()

	ptrs := make(chan *Context, 50)

	app.Get("/test", func(ctx *Context) {
		child, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		_ = child

		ptrs <- ctx
	})

	client := app.TestClient()
	for range cap(ptrs) {
		resp, err := client.Get("/test")
		qt.Assert(t, qt.IsNil(err))
		fasthttp.ReleaseResponse(resp)
	}

	close(ptrs)

	distinct := make(map[*Context]struct{})
	for ctx := range ptrs {
		distinct[ctx] = struct{}{}
	}

	// Any reuse at all suffices: sync.Pool drops a fraction of Puts under
	// -race, while a Context never returned to the pool yields zero reuse.
	qt.Check(t, qt.IsTrue(len(distinct) < cap(ptrs)),
		qt.Commentf("Context pool defeated: %d distinct instances for %d requests", len(distinct), cap(ptrs)))
}

// A handler that leaks its cancel must not leak a stdlib watcher goroutine.
func TestDerivedContextNoWatcherGoroutine(t *testing.T) {
	app := NewTestApp()

	app.Start(t)
	defer app.Stop()

	cancels := make(chan context.CancelFunc, 101)

	app.Get("/test", func(ctx *Context) {
		_, cancel := context.WithCancel(ctx)
		cancels <- cancel
	})

	client := app.TestClient()

	resp, err := client.Get("/test")
	qt.Assert(t, qt.IsNil(err))
	fasthttp.ReleaseResponse(resp)

	before := runtime.NumGoroutine()

	for range 100 {
		resp, err := client.Get("/test")
		qt.Assert(t, qt.IsNil(err))
		fasthttp.ReleaseResponse(resp)
	}

	after := runtime.NumGoroutine()
	qt.Check(t, qt.IsTrue(after < before+50),
		qt.Commentf("watcher goroutines leaked: %d before, %d after", before, after))

	close(cancels)

	for cancel := range cancels {
		cancel()
	}
}

// context.AfterFunc on the request Context runs at request end unless stopped.
func TestContextAfterFunc(t *testing.T) {
	app := NewTestApp()

	app.Start(t)
	defer app.Stop()

	ran := make(chan struct{}, 1)

	app.Get("/test", func(ctx *Context) {
		stop := context.AfterFunc(ctx, func() { t.Error("stopped AfterFunc ran") })
		qt.Check(t, qt.IsTrue(stop()))

		context.AfterFunc(ctx, func() { ran <- struct{}{} })
	})

	resp, err := app.TestClient().Get("/test")
	qt.Assert(t, qt.IsNil(err))
	fasthttp.ReleaseResponse(resp)

	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("AfterFunc did not run at request end")
	}
}

// Canceling a cancellable context installed via SetContext cancels the
// request Context and its derived children.
func TestSetContextCancellationPropagates(t *testing.T) {
	app := NewTestApp()

	app.Start(t)
	defer app.Stop()

	app.Get("/test", func(ctx *Context) {
		ictx, cancel := context.WithCancel(ctx.Context())
		defer cancel()

		ctx.SetContext(ictx)

		child, childCancel := context.WithCancel(ctx)
		defer childCancel()

		cancel()

		select {
		case <-child.Done():
			ctx.StatusCode(http.StatusOK)
		case <-time.After(time.Second):
			ctx.StatusCode(http.StatusInternalServerError)
		}
	})

	resp, err := app.TestClient().Get("/test")
	qt.Assert(t, qt.IsNil(err))
	defer fasthttp.ReleaseResponse(resp)

	qt.Check(t, qt.Equals(resp.StatusCode(), http.StatusOK))
}

// In-flight request contexts are canceled when the application stops.
func TestContextCanceledOnShutdown(t *testing.T) {
	app := NewTestApp()

	app.Start(t)

	entered := make(chan struct{})
	result := make(chan error, 1)

	app.Get("/test", func(ctx *Context) {
		done := ctx.Done()

		close(entered)

		select {
		case <-done:
			result <- ctx.Err()
		case <-time.After(3 * time.Second):
			result <- nil
		}
	})

	go func() {
		resp, err := app.TestClient().Get("/test")
		if err == nil {
			fasthttp.ReleaseResponse(resp)
		}
	}()

	<-entered
	app.Stop()

	qt.Check(t, qt.ErrorIs(<-result, context.Canceled))
}

// A cancellable child derived from the request Context must not race the
// Context release. Passes trivially without -race.
func TestDerivedCancellableContextDoesNotRaceRelease(t *testing.T) {
	app := NewTestApp()

	app.Start(t)
	defer app.Stop()

	app.Get("/test", func(ctx *Context) {
		child, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		_ = child
	})

	for range 20 {
		resp, err := app.TestClient().Get("/test")
		qt.Assert(t, qt.IsNil(err))
		fasthttp.ReleaseResponse(resp)
	}
}
