package coroutine

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/net/context"
)

// For troubleshooting stack pretty printing only.
// Set to true to see full stack trace that includes framework methods.
const disableCleanStackTraces = false

type valueCallbackPair struct {
	value    interface{}
	callback func()
}

type channelImpl struct {
	name         string              // human readable channel name
	size         int                 // Channel buffer size. 0 for non buffered.
	buffer       []interface{}       // buffered messages
	blockedSends []valueCallbackPair // puts waiting when buffer is full.
	blockedRecvs []func(interface{}) // receives waiting when no messages are available.
	closed       bool                // true if channel is closed
}

// Single case statement of the Select
type selectCase struct {
	channel   Channel       // Channel of this case.
	recvFunc  *RecvCaseFunc // function to call when channel has a message. nil for send case.
	sendFunc  *SendCaseFunc // function to call when channel accepted a message. nil for receive case.
	sendValue *interface{}  // value to send to the channel. Used only for send case.
}

// Implements Selector interface
type selectorImpl struct {
	name        string
	cases       []selectCase     // cases that this select is comprised from
	defaultFunc *DefaultCaseFunc // default case
}

// unblockFunc is passed evaluated by a coroutine yield. When it returns false the yield returns to a caller.
// stackDepth is the depth of stack from the last blocking call relevant to user.
// Used to truncate internal stack frames from thread stack.
type unblockFunc func(status string, stackDepth int) (keepBlocked bool)

type coroutineState struct {
	context.Context
	name         string
	dispatcher   *dispatcherImpl  // dispatcher this context belongs to
	aboutToBlock chan bool        // used to notify dispatcher that coroutine that owns this context is about to block
	unblock      chan unblockFunc // used to notify coroutine that it should continue executing.
	keptBlocked  bool             // true indicates that coroutine didn't make any progress since the last yield unblocking
	closed       bool             // indicates that owning coroutine has finished execution
	panicError   PanicError       // non nil if coroutine had unhandled panic
}

type panicError struct {
	value      interface{}
	stackTrace string
}

type dispatcherImpl struct {
	sequence         int
	channelSequence  int // used to name channels
	selectorSequence int // used to name channels
	coroutines       []*coroutineState
	executing        bool       // currently running ExecuteUntilAllBlocked. Used to avoid recursive calls to it.
	mutex            sync.Mutex // used to synchronize executing
	closed           bool
}

// Assert that structs do indeed implement the interfaces
var _ Channel = (*channelImpl)(nil)
var _ Selector = (*selectorImpl)(nil)
var _ Dispatcher = (*dispatcherImpl)(nil)
var _ PanicError = (*panicError)(nil)

func (c *channelImpl) Recv(ctx Context) (v interface{}, more bool) {
	ctxImpl := ctx.Value(contextKey).(*coroutineState)
	hasResult := false
	var result interface{}
	for {
		if hasResult {
			ctxImpl.unblocked()
			return result, true
		}
		if len(c.buffer) > 0 {
			r := c.buffer[0]
			c.buffer = c.buffer[1:]
			ctxImpl.unblocked()
			return r, true
		}
		if c.closed {
			return nil, false
		}
		if len(c.blockedSends) > 0 {
			b := c.blockedSends[0]
			c.blockedSends = c.blockedSends[1:]
			b.callback()
			ctxImpl.unblocked()
			return b.value, true
		}
		c.blockedRecvs = append(c.blockedRecvs, func(v interface{}) {
			result = v
			hasResult = true
		})
		ctxImpl.yield(fmt.Sprintf("blocked on %s.Recv", c.name))
	}
}

func (c *channelImpl) RecvAsync() (v interface{}, ok bool, more bool) {
	if len(c.buffer) > 0 {
		r := c.buffer[0]
		c.buffer = c.buffer[1:]
		return r, true, true
	}
	if c.closed {
		return nil, false, false
	}
	if len(c.blockedSends) > 0 {
		b := c.blockedSends[0]
		c.blockedSends = c.blockedSends[1:]
		b.callback()
		return b.value, true, true
	}
	return nil, false, true
}

func (c *channelImpl) Send(ctx Context, v interface{}) {
	ctxImpl := ctx.Value(contextKey).(*coroutineState)
	valueConsumed := false
	for {
		// Check for closed in the loop as close can be called when send is blocked
		if c.closed {
			panic("Closed channel")
		}
		if valueConsumed {
			ctxImpl.unblocked()
			return
		}
		if len(c.buffer) < c.size {
			c.buffer = append(c.buffer, v)
			ctxImpl.unblocked()
			return
		}
		if len(c.blockedRecvs) > 0 {
			blockedGet := c.blockedRecvs[0]
			c.blockedRecvs = c.blockedRecvs[1:]
			blockedGet(v)
			ctxImpl.unblocked()
			return
		}
		c.blockedSends = append(c.blockedSends,
			valueCallbackPair{value: v, callback: func() { valueConsumed = true }})
		ctxImpl.yield(fmt.Sprintf("blocked on %s.Send", c.name))
	}
}

func (c *channelImpl) SendAsync(v interface{}) (ok bool) {
	if c.closed {
		panic("Closed channel")
	}
	if len(c.buffer) < c.size {
		c.buffer = append(c.buffer, v)
		return true
	}
	if len(c.blockedRecvs) > 0 {
		blockedGet := c.blockedRecvs[0]
		c.blockedRecvs = c.blockedRecvs[1:]
		blockedGet(v)
		return true
	}
	return false
}

func (c *channelImpl) Close() {
	c.closed = true
	// All blocked sends are going to panic
	for i := 0; i < len(c.blockedSends); i++ {
		b := c.blockedSends[i]
		b.callback()
	}
}

// initialYield called at the beginning of the coroutine execution
// stackDepth is the depth of top of the stack to omit when stack trace is generated
// to hide frames internal to the framework.
func (s *coroutineState) initialYield(stackDepth int, status string) {
	keepBlocked := true
	for keepBlocked {
		f := <-s.unblock
		keepBlocked = f(status, stackDepth+1)
	}
}

// yield indicates that coroutine cannot make progress and should sleep
// this call blocks
func (s *coroutineState) yield(status string) {
	s.aboutToBlock <- true
	s.initialYield(3, status) // omit three levels of stack. To adjust change to 0 and count the lines to remove.
	s.keptBlocked = true
}

func getStackTrace(coroutineName, status string, stackDepth int) string {
	stack := stackBuf[:runtime.Stack(stackBuf[:], false)]
	rawStack := fmt.Sprintf("%s", strings.TrimRightFunc(string(stack), unicode.IsSpace))
	if disableCleanStackTraces {
		return rawStack
	}
	lines := strings.Split(rawStack, "\n")
	// Omit top stackDepth frames + top status line.
	// Omit bottom two frames which is wrapping of coroutine in a goroutine.
	lines = lines[stackDepth*2+1 : len(lines)-4]
	top := fmt.Sprintf("coroutine %s [%s]:", coroutineName, status)
	lines = append([]string{top}, lines...)
	return strings.Join(lines, "\n")
}

// unblocked is called by coroutine to indicate that since the last time yield was unblocked channel or select
// where unblocked versus calling yield again after checking their condition
func (s *coroutineState) unblocked() {
	s.keptBlocked = false
}

func (s *coroutineState) call() {
	s.unblock <- func(status string, stackDepth int) bool {
		return false // unblock
	}
	<-s.aboutToBlock
}

func (s *coroutineState) close() {
	s.closed = true
	s.aboutToBlock <- true
}

func (s *coroutineState) exit() {
	if !s.closed {
		s.unblock <- func(status string, stackDepth int) bool {
			runtime.Goexit()
			return true
		}
	}
}

var stackBuf [100000]byte

func (s *coroutineState) stackTrace() string {
	if s.closed {
		return ""
	}
	stackCh := make(chan string, 1)
	s.unblock <- func(status string, stackDepth int) bool {
		stackCh <- getStackTrace(s.name, status, stackDepth+1)
		return true
	}
	return <-stackCh
}

func (s *coroutineState) NewCoroutine(ctx Context, f Func) {
	s.dispatcher.newCoroutine(ctx, f)
}

func (s *coroutineState) NewNamedCoroutine(ctx Context, name string, f Func) {
	s.dispatcher.newNamedCoroutine(ctx, name, f)
}

func (s *coroutineState) NewSelector() Selector {
	s.dispatcher.selectorSequence++
	return s.NewNamedSelector(fmt.Sprintf("selector-%v", s.dispatcher.selectorSequence))
}

func (s *coroutineState) NewNamedSelector(name string) Selector {
	return &selectorImpl{name: name}
}

func (s *coroutineState) NewChannel() Channel {
	s.dispatcher.channelSequence++
	return s.NewNamedChannel(fmt.Sprintf("chan-%v", s.dispatcher.channelSequence))
}

func (s *coroutineState) NewNamedChannel(name string) Channel {
	return &channelImpl{name: name}
}

func (s *coroutineState) NewBufferedChannel(size int) Channel {
	return &channelImpl{size: size}
}

func (s *coroutineState) NewNamedBufferedChannel(name string, size int) Channel {
	return &channelImpl{name: name, size: size}
}

func (e *panicError) Error() string {
	return fmt.Sprintf("%v", e.value)
}

func (e *panicError) Value() interface{} {
	return e.value
}

func (e *panicError) StackTrace() string {
	return e.stackTrace
}

func (d *dispatcherImpl) newCoroutine(ctx Context, f Func) {
	d.newNamedCoroutine(ctx, fmt.Sprintf("%v", d.sequence+1), f)
}

func (d *dispatcherImpl) newNamedCoroutine(ctx Context, name string, f Func) {
	state := d.newState(name)
	spawned := WithValue(ctx, contextKey, state)
	go func(crt *coroutineState) {
		defer crt.close()
		defer func() {
			if r := recover(); r != nil {
				st := getStackTrace(name, "panic", 3)
				crt.panicError = &panicError{value: r, stackTrace: st}
			}
		}()
		crt.initialYield(1, "")
		f(spawned)
	}(state)
}

func (d *dispatcherImpl) newState(name string) *coroutineState {
	c := &coroutineState{
		Context:      context.Background(),
		name:         name,
		dispatcher:   d,
		aboutToBlock: make(chan bool, 1),
		unblock:      make(chan unblockFunc),
	}
	d.sequence++
	d.coroutines = append(d.coroutines, c)
	return c
}

func (d *dispatcherImpl) ExecuteUntilAllBlocked() (err PanicError) {
	d.mutex.Lock()
	if d.closed {
		panic("dispatcher is closed")
	}
	if d.executing {
		panic("call to ExecuteUntilAllBlocked (possibly from a coroutine) while it is already running")
	}
	d.executing = true
	d.mutex.Unlock()
	defer func() { d.executing = false }()
	allBlocked := false
	// Keep executing until at least one goroutine made some progress
	for !allBlocked {
		// Give every coroutine chance to execute removing closed ones
		allBlocked = true
		lastSequence := d.sequence
		for i := 0; i < len(d.coroutines); i++ {
			c := d.coroutines[i]
			if !c.closed {
				// TODO: Support handling of panic in a coroutine by dispatcher.
				// TODO: Dump all outstanding coroutines if one of them panics
				c.call()
			}
			// c.call() can close the context so check again
			if c.closed {
				// remove the closed one from the slice
				d.coroutines = append(d.coroutines[:i],
					d.coroutines[i+1:]...)
				i--
				if c.panicError != nil {
					return c.panicError
				}
				allBlocked = false

			} else {
				allBlocked = allBlocked && (c.keptBlocked || c.closed)
			}
		}
		// Set allBlocked to false if new coroutines where created
		allBlocked = allBlocked && lastSequence == d.sequence
		if len(d.coroutines) == 0 {
			break
		}
	}
	return nil
}

func (d *dispatcherImpl) IsDone() bool {
	return len(d.coroutines) == 0
}

func (d *dispatcherImpl) Close() {
	d.mutex.Lock()
	d.closed = true
	d.mutex.Unlock()
	for i := 0; i < len(d.coroutines); i++ {
		c := d.coroutines[i]
		if !c.closed {
			c.exit()
		}
	}
}

func (d *dispatcherImpl) StackTrace() string {
	var result string
	for i := 0; i < len(d.coroutines); i++ {
		c := d.coroutines[i]
		if !c.closed {
			if len(result) > 0 {
				result += "\n\n"
			}
			result += c.stackTrace()
		}
	}
	return result
}

func (s *selectorImpl) AddRecv(c Channel, f RecvCaseFunc) Selector {
	s.cases = append(s.cases, selectCase{channel: c, recvFunc: &f})
	return s
}

func (s *selectorImpl) AddSend(c Channel, v interface{}, f SendCaseFunc) Selector {
	s.cases = append(s.cases, selectCase{channel: c, sendFunc: &f, sendValue: &v})
	return s
}

func (s *selectorImpl) AddDefault(f DefaultCaseFunc) {
	s.defaultFunc = &f
}

func (s *selectorImpl) Select(ctx Context) {
	ctxImpl := ctx.Value(contextKey).(*coroutineState)
	for {
		for _, pair := range s.cases {
			if pair.recvFunc != nil {
				v, ok, more := pair.channel.RecvAsync()
				if ok || !more {
					f := *pair.recvFunc
					f(v, more)
					ctxImpl.unblocked()
					return
				}
			} else {
				ok := pair.channel.SendAsync(*pair.sendValue)
				if ok {
					f := *pair.sendFunc
					f()
					ctxImpl.unblocked()
					return
				}
			}
		}
		if s.defaultFunc != nil {
			f := *s.defaultFunc
			f()
			ctxImpl.unblocked()
			return
		}
		ctxImpl.yield(fmt.Sprintf("blocked on %s.Select", s.name))
	}
}
