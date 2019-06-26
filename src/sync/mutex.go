// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	state int32
	sema  uint32
}

/*
state 32位，一共用来表示四个数据, 0位表示是否加锁，1位表示是否唤醒，
2位表示是否饥饿模式, 3到31表示堵塞在该互斥锁上的goroutine数量
示意图如下:
state: |31|30|29|...|3|2|1|0|
        \_____________/| | |
               |       | | |
               |       | | mutex是否加锁，1加锁，0未加锁
               |       | mutex是否被唤醒
               |       mutex是否为饥饿模式
               |
               当前堵塞在该互斥锁上的goroutine数量
sema 是一个非负数的信号量
*/

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // mutex is locked
	mutexWoken
	mutexStarving
	mutexWaiterShift = iota
	/*
		mutexLocked = 1
		mutexWoken = 2
		mutexStarving = 4
		mutexWaiterShift = 3
	*/

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	/*
		互斥锁有两种模式: 正常模式和饥饿模式。再正常模式下所有等待加锁的goroutine在队列中
		按照先后顺序等待, 一个被唤醒的之前在等待的goroutine, 不会直接获得锁，而是要和新来
		的进行加锁的goroutine进行竞争。新来的goroutine有个优势: 他们正在占用着cpu, 并且可
		能 有多个，所以被唤醒的goroutine很可能再竞争中失败， 这种情况下，被唤醒的goroutine
		会被 加到队列的前面。 如果一个goroutine超过1毫秒还未获得到锁，那么他会把互斥锁转换
		为饥饿模式。
	*/

	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	/*
		在饥饿模式下，互斥锁的所有权会直接从调用unlock的goroutine交给等待队列的第一个goroutine。
		新来进行加锁的goroutine不会尝试去获取这个互斥锁，尽管这个锁是unlocked状态，也不会去做
		自旋。相反，这些goroutine会把自己加到等待队列的尾部。
	*/

	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	/*
		如果一个等待的goroutine获取到了互斥锁，并且满足如下任一个条件时，会把这个锁转换为
		正常模式。
		(1) 它在等待队列的尾部，
		(2) 它的等待时间小于1毫秒。
	*/
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.

	/*
		正常模式下具有相当好的性能，因为即使存在堵塞的等待者，一个goroutine也可以连续
		多次获得互斥锁。饥饿模式对于防止尾部延迟现象是很重要的。
	*/

	starvationThresholdNs = 1e6
	// 请求加锁的goroutine等待多长时间后尝试把互斥锁设置为饥饿模式， 1ms
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 调用cas原子操作直接获取一个正常状态未加锁的互斥锁, old=0表示互斥锁未被加锁&&未被唤醒&&正常模式&&无等待队列
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	//协程加锁等待时间
	var waitStartTime int64
	// 是否饥饿模式
	starving := false
	// 是否唤醒
	awoke := false
	// 自旋次数
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// 饥饿模式下不做自旋，所有权被转给等待者
		// so we won't be able to acquire the mutex anyway.
		// 所以我们不能获取互斥锁
		// if第一个条件时当前锁的为加锁并且非饥饿模式
		// if第二个条件是判断当前进行自旋是否有意义
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// 主动的自旋是有意义的
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			/*
				尝试把互斥锁设置为被唤醒, 以通知进行unlock的goroutine不要唤醒其他堵塞
				的goroutine
			*/
			/*
				当前goroutine未唤醒&&互斥锁未唤醒&&等待队列非空&&可以通过cas设置互斥锁为
				唤醒时， 把当前goroutine设置为唤醒
			*/
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 不能尝试获取饥饿模式下的互斥锁，新到的goroutine必须排队
		// 非饥饿模式下才会去尝试加锁
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		// 互斥锁已被获取或者在饥饿模式下,增加等待者数量
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 当前goroutine已经是饥饿状态，并且mutex已经被加锁时，把互斥锁设置为饥饿模式
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		// 如果当前goroutine是唤醒状态的话需要把互斥锁设置为未唤醒
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}
		// cas 设置互斥锁的state为new, 设置不成功old=m.state继续如上循环
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 若果锁之前是未加锁，并且非饥饿状态的话cas加锁成功
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 如果当前goroutine之前已经在等待了，需要把当前goroutine放到队列头queuelifo=true
			// 新来的goroutine 放到队列尾， queuelifo=fase
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// 等待时间超过1ms后把当前 goroutine设置为饥饿状态
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				/*
					如果锁已经是饥饿状态，则把锁的所有权直接交给当前goroutine
					但是存在一些非法状态: 当前锁已被加锁或者已被唤醒 或者等待队列非空
				*/
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 当前goroutine通过原子操作add来获取锁，并且把等待者数量减一。
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 当前goroutine非饥饿状态或者只有一个等待者时，把互斥锁设置为正常模式
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
/*
互斥锁被一个goroutine加锁之后，是允许其他goroutine进行解锁此互斥锁的。
*/
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		/*
			解锁之后，new!=0说明，解锁之后，互斥锁可能已经被唤醒或者处于饥饿模式或者等待队列非空
		*/
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 若进行解锁的互斥锁不是锁定状态，报fatal异常，无法被recover住
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	// 若互斥锁非饥饿模式
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			/*
				如果互斥锁等待队列为空，或者互斥锁已经被唤醒，或者互斥锁已经加锁或者互斥锁处于
				饥饿模式，就不需要再唤醒某个goroutine, 直接返回。
			*/
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 抓住权利去唤醒某个goroutine

			new = (old - 1<<mutexWaiterShift) | mutexWoken
			// state的新值需要把等待goroutine的计数减1，并且设置为被唤醒
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		/*
			饥饿模式直接把互斥锁的所有权转给下一个等待的goroutine
		*/
		runtime_Semrelease(&m.sema, true, 1)
	}
}
