package azugo

import (
	"context"
	"time"
)

// requestContextKeyType is the type of the sentinel key under which a Context
// returns itself from Value, so RequestContext can recover the *Context even
// after it has been wrapped by other context.Context decorators.
type requestContextKeyType struct{}

var requestContextKey requestContextKeyType

// SetContext installs the effective request context.
//
// ctx MUST be derived from c.Context() (the underlying request context),
// for example using context.WithValue(c.Context(), key, value).
//
// Passing a nil ctx resets the effective context back to the base request
// context.
func (c *Context) SetContext(ctx context.Context) {
	if c.reqCtxStop != nil {
		c.reqCtxStop()
		c.reqCtxStop = nil
	}

	c.reqCtx = ctx

	if ctx == nil {
		return
	}

	// Bridge cancellation of the installed context to the request lifecycle
	if d := ctx.Done(); d != nil && (c.context == nil || d != c.context.Done()) {
		c.cancelMu.Lock()
		c.touched.Store(true)
		gen := c.gen
		c.cancelMu.Unlock()

		c.reqCtxStop = context.AfterFunc(ctx, func() {
			c.cancel(gen, ctx.Err())
		})
	}
}

func (c *Context) effectiveContext() context.Context {
	if c.reqCtx != nil {
		return c.reqCtx
	}

	if c.app.ctxExt != nil {
		if ce := c.app.ctxExt.Context(c); ce != nil && ce != c {
			return ce
		}
	}

	return c.context
}

// Deadline returns the time when work done on behalf of this context
// should be canceled. Deadline returns ok==false when no deadline is
// set. Successive calls to Deadline return the same results.
func (c *Context) Deadline() (time.Time, bool) {
	if c == nil || c.context == nil {
		return time.Time{}, false
	}

	return c.effectiveContext().Deadline()
}

// Done returns a channel that is closed when the request completes, the
// server is shutting down or a cancellable context installed via SetContext
// is canceled. The channel is created lazily and is safe to hand to code
// that outlives the handler.
func (c *Context) Done() <-chan struct{} {
	if c == nil {
		return nil
	}

	if d := c.doneCh.Load(); d != nil {
		return *d
	}

	c.cancelMu.Lock()

	d := c.doneCh.Load()
	if d == nil {
		c.touched.Store(true)

		ch := make(chan struct{})
		if c.ctxErr != nil {
			close(ch)
		}

		c.doneCh.Store(&ch)
		d = &ch
	}

	c.cancelMu.Unlock()

	c.markLive()

	return *d
}

// Err returns nil while the request is being served and a non-nil error
// after the Context has been canceled.
func (c *Context) Err() error {
	if c == nil {
		return nil
	}

	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()

	return c.ctxErr
}

// AfterFunc registers f to run once the Context is canceled and returns a
// stop function reporting whether it removed the registration.
func (c *Context) AfterFunc(f func()) func() bool {
	c.cancelMu.Lock()

	c.touched.Store(true)

	if c.ctxErr != nil {
		c.pendingCancels++
		c.cancelMu.Unlock()

		go func() {
			f()

			c.donePendingCancel()
		}()

		return func() bool { return false }
	}

	if c.afterFuncs == nil {
		c.afterFuncs = make(map[*func()]struct{})
	}

	k := &f
	c.afterFuncs[k] = struct{}{}

	c.cancelMu.Unlock()

	c.markLive()

	return func() bool {
		c.cancelMu.Lock()
		defer c.cancelMu.Unlock()

		_, ok := c.afterFuncs[k]
		delete(c.afterFuncs, k)

		return ok
	}
}

// cancel ends the request lifecycle: Err starts returning err, the done
// channel is closed and registered after funcs run.
func (c *Context) cancel(gen uint64, err error) {
	c.cancelMu.Lock()

	if c.ctxErr != nil || gen != c.gen {
		c.cancelMu.Unlock()

		return
	}

	c.ctxErr = err

	if d := c.doneCh.Load(); d != nil {
		close(*d)
	}

	funcs := c.afterFuncs
	c.afterFuncs = nil

	if funcs == nil {
		c.cancelMu.Unlock()

		return
	}

	// Canceling a child re-enters Err and Value
	c.pendingCancels++
	c.cancelMu.Unlock()

	for f := range funcs {
		(*f)()
	}

	c.donePendingCancel()
}

// donePendingCancel marks one canceling goroutine as finished.
func (c *Context) donePendingCancel() {
	c.cancelMu.Lock()

	c.pendingCancels--
	if c.pendingCancels == 0 && c.cancelIdle != nil {
		c.cancelIdle.Broadcast()
	}

	c.cancelMu.Unlock()
}

// markLive registers the Context to be canceled on server shutdown.
func (c *Context) markLive() {
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()

	if c.live || c.ctxErr != nil || c.app == nil {
		return
	}

	c.live = true

	c.app.ctxLiveMu.Lock()
	c.app.ctxLive[c] = c.gen
	c.app.ctxLiveMu.Unlock()
}

// Value returns the value associated with this context for key, or nil
// if no value is associated with key. Successive calls to Value with
// the same key returns the same result.
//
// Use context values only for request-scoped data that transits
// processes and API boundaries, not for passing optional parameters to
// functions.
//
// A key identifies a specific value in a Context. Functions that wish
// to store values in Context typically allocate a key in a global
// variable then use that key as the argument to context.WithValue and
// Context.Value. A key can be any type that supports equality;
// packages should define keys as an unexported type to avoid
// collisions.
//
// Packages that define a Context key should provide type-safe accessors
// for the values stored using that key:
//
//	// Package user defines a User type that's stored in Contexts.
//	package user
//
//	import "context"
//
//	// User is the type of value stored in the Contexts.
//	type User struct {...}
//
//	// key is an unexported type for keys defined in this package.
//	// This prevents collisions with keys defined in other packages.
//	type key int
//
//	// userKey is the key for user.User values in Contexts. It is
//	// unexported; clients use user.NewContext and user.FromContext
//	// instead of using this key directly.
//	var userKey key
//
//	// NewContext returns a new Context that carries value u.
//	func NewContext(ctx context.Context, u *User) context.Context {
//		return context.WithValue(ctx, userKey, u)
//	}
//
//	// FromContext returns the User value stored in ctx, if any.
//	func FromContext(ctx context.Context) (*User, bool) {
//		u, ok := ctx.Value(userKey).(*User)
//		return u, ok
//	}
func (c *Context) Value(key any) any {
	if c == nil || c.context == nil {
		return nil
	}

	if key == requestContextKey {
		return c
	}

	if c.reqCtx != nil {
		return c.reqCtx.Value(key)
	}

	if c.app.ctxExt != nil {
		if ce := c.app.ctxExt.Context(c); ce != nil && ce != c {
			if v := ce.Value(key); v != nil {
				return v
			}
		}
	}

	return c.context.Value(key)
}

// Contexter is an interface that checks if type has a request context.
type Contexter interface {
	// RequestContext returns request context.
	RequestContext() context.Context
}

// RequestContext returns the request context carried by ctx, or nil if
// there is none.
//
//nolint:contextcheck
func RequestContext(ctx context.Context) *Context {
	if ctx == nil {
		return nil
	}

	if c, ok := ctx.(Contexter); ok {
		ctx = c.RequestContext()
		if ctx == nil {
			return nil
		}
	}

	if rctx, ok := ctx.(*Context); ok {
		return rctx
	}

	// Recover through arbitrary wrappers that delegate Value up the chain.
	if rctx, ok := ctx.Value(requestContextKey).(*Context); ok {
		return rctx
	}

	return nil
}

// ExtendedContext is an interface that can be implemented to extend the context.
//
// Deprecated: use Context.SetContext to install the effective request context
// instead.
type ExtendedContext interface {
	// Context returns extended context.
	Context(ctx context.Context) context.Context
}
