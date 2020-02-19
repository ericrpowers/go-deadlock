package deadlock

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/petermattis/goid"
	"go.uber.org/zap"
)

// Opts control how deadlock detection behaves.
// Options are supposed to be set once at a startup (say, when parsing flags).
var Opts = struct {
	// Mutex/RWMutex would work exactly as their sync counterparts
	// -- almost no runtime penalty, no deadlock detection if Disable == true.
	Disable bool
	// Would disable lock order based deadlock detection if DisableLockOrderDetection == true.
	DisableLockOrderDetection bool
	// Waiting for a lock for longer than DeadlockTimeout is considered a deadlock.
	// Ignored is DeadlockTimeout <= 0.
	DeadlockTimeout time.Duration
	// OnPotentialDeadlock is called each time a potential deadlock is detected -- either based on
	// lock order or on lock wait time.
	OnPotentialDeadlock func()
	// Will keep MaxMapSize lock pairs (happens before // happens after) in the map.
	// The map resets once the threshold is reached.
	MaxMapSize int
	// Will dump stacktraces of all goroutines when inconsistent locking is detected.
	PrintAllCurrentGoroutines bool
	mu                        *sync.Mutex // Protects the LogBuf.
	// Will print deadlock info to log buffer.
	LogBuf io.Writer
}{
	DeadlockTimeout: time.Second * 30,
	OnPotentialDeadlock: func() {
		os.Exit(2)
	},
	MaxMapSize: 1024 * 64,
	mu:         &sync.Mutex{},
	LogBuf:     os.Stderr,
}

// Cond is sync.Cond wrapper
type Cond struct {
	sync.Cond
}

// Locker is sync.Locker wrapper
type Locker struct {
	sync.Locker
}

// Once is sync.Once wrapper
type Once struct {
	sync.Once
}

// Pool is sync.Poll wrapper
type Pool struct {
	sync.Pool
}

// WaitGroup is sync.WaitGroup wrapper
type WaitGroup struct {
	sync.WaitGroup
}

// A Mutex is a drop-in replacement for sync.Mutex.
// Performs deadlock detection unless disabled in Opts.
type Mutex struct {
	mu sync.Mutex
}

// Lock locks the mutex.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
//
// Unless deadlock detection is disabled, logs potential deadlocks,
// calling Opts.OnPotentialDeadlock on each occasion.
func (m *Mutex) Lock() {
	lock(m.mu.Lock, m)
}

// Unlock unlocks the mutex.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	m.mu.Unlock()
	if !Opts.Disable {
		postUnlock(m)
	}
}

// An RWMutex is a drop-in replacement for sync.RWMutex.
// Performs deadlock detection unless disabled in Opts.
type RWMutex struct {
	mu sync.RWMutex
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
// To ensure that the lock eventually becomes available,
// a blocked Lock call excludes new readers from acquiring
// the lock.
//
// Unless deadlock detection is disabled, logs potential deadlocks,
// calling Opts.OnPotentialDeadlock on each occasion.
func (m *RWMutex) Lock() {
	lock(m.mu.Lock, m)
}

// Unlock unlocks the mutex for writing.  It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine.  One goroutine may RLock (Lock) an RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
func (m *RWMutex) Unlock() {
	m.mu.Unlock()
	if !Opts.Disable {
		postUnlock(m)
	}
}

// RLock locks the mutex for reading.
//
// Unless deadlock detection is disabled, logs potential deadlocks,
// calling Opts.OnPotentialDeadlock on each occasion.
func (m *RWMutex) RLock() {
	lock(m.mu.RLock, m)
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
func (m *RWMutex) RUnlock() {
	m.mu.RUnlock()
	if !Opts.Disable {
		postUnlock(m)
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling RLock and RUnlock.
func (m *RWMutex) RLocker() sync.Locker {
	return (*rlocker)(m)
}

func preLock(skip int, p interface{}) {
	lo.preLock(skip, p)
}

func postLock(skip int, p interface{}) {
	lo.postLock(skip, p)
}

func postUnlock(p interface{}) {
	lo.postUnlock(p)
}

func lock(lockFn func(), ptr interface{}) {
	if Opts.Disable {
		lockFn()
		return
	}
	preLock(4, ptr)
	if Opts.DeadlockTimeout <= 0 {
		lockFn()
	} else {
		ch := make(chan struct{})
		go func() {
			for {
				t := time.NewTimer(Opts.DeadlockTimeout)
				defer t.Stop() // This runs after the losure finishes, but it's OK.
				select {
				case <-t.C:
					lo.mu.Lock()
					prev, ok := lo.cur[ptr]
					if !ok {
						lo.mu.Unlock()
						break // Nobody seems to be holding the lock, try again.
					}
					Opts.mu.Lock()
					zap.L().Warn(header)
					zap.L().Warn("Previous place where the lock was grabbed")
					zap.L().Warn(fmt.Sprintf("goroutine %v lock %p\n", prev.gid, ptr))
					printStack(prev.stack)
					zap.L().Warn(fmt.Sprintf("Have been trying to lock it again for more than %d", Opts.DeadlockTimeout))
					zap.L().Warn(fmt.Sprintf("goroutine %v lock %p\n", goid.Get(), ptr))
					printStack(callers(2))
					stacks := stacks()
					grs := bytes.Split(stacks, []byte("\n\n"))
					for _, g := range grs {
						if goid.ExtractGID(g) == prev.gid {
							zap.L().Warn(fmt.Sprintf("Here is what goroutine %d doing now", prev.gid))
						}
					}
					lo.other(ptr)
					if Opts.PrintAllCurrentGoroutines {
						zap.L().Warn("All current goroutines:", zap.ByteString("stacks", stacks))
					}
					Opts.mu.Unlock()
					lo.mu.Unlock()
					Opts.OnPotentialDeadlock()
					zap.L().Fatal("Forcing fatal...")
					<-ch
					return
				case <-ch:
					return
				}
			}
		}()
		lockFn()
		postLock(4, ptr)
		close(ch)
		return
	}
	postLock(4, ptr)
}

type lockOrder struct {
	mu    sync.Mutex
	cur   map[interface{}]stackGID // stacktraces + gids for the locks currently taken.
	order map[beforeAfter]ss       // expected order of locks.
}

type stackGID struct {
	stack []uintptr
	gid   int64
}

type beforeAfter struct {
	before interface{}
	after  interface{}
}

type ss struct {
	before []uintptr
	after  []uintptr
}

var lo = newLockOrder()

func newLockOrder() *lockOrder {
	return &lockOrder{
		cur:   map[interface{}]stackGID{},
		order: map[beforeAfter]ss{},
	}
}

func (l *lockOrder) postLock(skip int, p interface{}) {
	stack := callers(skip)
	gid := goid.Get()
	l.mu.Lock()
	l.cur[p] = stackGID{stack, gid}
	l.mu.Unlock()
}

func (l *lockOrder) preLock(skip int, p interface{}) {
	if Opts.DisableLockOrderDetection {
		return
	}
	stack := callers(skip)
	gid := goid.Get()
	l.mu.Lock()
	for b, bs := range l.cur {
		if b == p {
			if bs.gid == gid {
				Opts.mu.Lock()
				zap.L().Warn(fmt.Sprintf("%s Recursive locking:", header))
				zap.L().Warn(fmt.Sprintf("current goroutine %d lock %p\n", gid, b))
				printStack(stack)
				zap.L().Warn("Previous place where the lock was grabbed (same goroutine)")
				printStack(bs.stack)
				l.other(p)
				Opts.mu.Unlock()
				Opts.OnPotentialDeadlock()
				zap.L().Fatal("Forcing fatal...")
			}
			continue
		}
		if bs.gid != gid { // We want locks taken in the same goroutine only.
			continue
		}
		if s, ok := l.order[beforeAfter{p, b}]; ok {
			Opts.mu.Lock()
			zap.L().Warn(fmt.Sprintf("%s Inconsistent locking. saw this ordering in one goroutine:", header))
			zap.L().Warn("happened before")
			printStack(s.before)
			zap.L().Warn("happened after")
			printStack(s.after)
			zap.L().Warn("in another goroutine: happened before")
			printStack(bs.stack)
			zap.L().Warn("happened after")
			printStack(stack)
			l.other(p)
			Opts.mu.Unlock()
			Opts.OnPotentialDeadlock()
			zap.L().Fatal("Forcing fatal...")
		}
		l.order[beforeAfter{b, p}] = ss{bs.stack, stack}
		if len(l.order) == Opts.MaxMapSize { // Reset the map to keep memory footprint bounded.
			l.order = map[beforeAfter]ss{}
		}
	}
	l.mu.Unlock()
}

func (l *lockOrder) postUnlock(p interface{}) {
	l.mu.Lock()
	delete(l.cur, p)
	l.mu.Unlock()
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }

// Under lo.mu Locked.
func (l *lockOrder) other(ptr interface{}) {
	empty := true
	for k := range l.cur {
		if k == ptr {
			continue
		}
		empty = false
	}
	if empty {
		return
	}
	zap.L().Warn("Other goroutines holding locks:")
	for k, pp := range l.cur {
		if k == ptr {
			continue
		}
		zap.L().Warn(fmt.Sprintf("goroutine %v lock %p\n", pp.gid, k))
		printStack(pp.stack)
	}
}

const header = "POTENTIAL DEADLOCK:"
