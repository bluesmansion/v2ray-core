// Package vmess contains the implementation of VMess protocol and transportation.
//
// VMess contains both inbound and outbound connections. VMess inbound is usually used on servers
// together with 'freedom' to talk to final destination, while VMess outbound is usually used on
// clients with 'socks' for proxying.
package vmess

import (
	"sync"
	"time"

	"v2ray.com/core/common/protocol"
	"v2ray.com/core/common/signal"
)

const (
	updateIntervalSec = 10
	cacheDurationSec  = 120
)

type idEntry struct {
	id             *protocol.ID
	userIdx        int
	lastSec        protocol.Timestamp
	lastSecRemoval protocol.Timestamp
}

type TimedUserValidator struct {
	sync.RWMutex
	running    bool
	validUsers []*protocol.User
	userHash   map[[16]byte]*indexTimePair
	ids        []*idEntry
	hasher     protocol.IDHash
	cancel     *signal.CancelSignal
}

type indexTimePair struct {
	index   int
	timeSec protocol.Timestamp
}

func NewTimedUserValidator(hasher protocol.IDHash) protocol.UserValidator {
	tus := &TimedUserValidator{
		validUsers: make([]*protocol.User, 0, 16),
		userHash:   make(map[[16]byte]*indexTimePair, 512),
		ids:        make([]*idEntry, 0, 512),
		hasher:     hasher,
		running:    true,
		cancel:     signal.NewCloseSignal(),
	}
	go tus.updateUserHash(updateIntervalSec * time.Second)
	return tus
}

func (this *TimedUserValidator) Release() {
	if !this.running {
		return
	}

	this.cancel.Cancel()
	this.cancel.WaitForDone()

	this.Lock()
	defer this.Unlock()

	if !this.running {
		return
	}

	this.running = false
	this.validUsers = nil
	this.userHash = nil
	this.ids = nil
	this.hasher = nil
	this.cancel = nil
}

func (this *TimedUserValidator) generateNewHashes(nowSec protocol.Timestamp, idx int, entry *idEntry) {
	var hashValue [16]byte
	var hashValueRemoval [16]byte
	idHash := this.hasher(entry.id.Bytes())
	for entry.lastSec <= nowSec {
		idHash.Write(entry.lastSec.Bytes(nil))
		idHash.Sum(hashValue[:0])
		idHash.Reset()

		idHash.Write(entry.lastSecRemoval.Bytes(nil))
		idHash.Sum(hashValueRemoval[:0])
		idHash.Reset()

		this.Lock()
		this.userHash[hashValue] = &indexTimePair{idx, entry.lastSec}
		delete(this.userHash, hashValueRemoval)
		this.Unlock()

		entry.lastSec++
		entry.lastSecRemoval++
	}
}

func (this *TimedUserValidator) updateUserHash(interval time.Duration) {
	this.cancel.WaitThread()
	defer this.cancel.FinishThread()

	for {
		select {
		case now := <-time.After(interval):
			nowSec := protocol.Timestamp(now.Unix() + cacheDurationSec)
			for _, entry := range this.ids {
				this.generateNewHashes(nowSec, entry.userIdx, entry)
			}
		case <-this.cancel.WaitForCancel():
			return
		}
	}
}

func (this *TimedUserValidator) Add(user *protocol.User) error {
	idx := len(this.validUsers)
	this.validUsers = append(this.validUsers, user)
	rawAccount, err := user.GetTypedAccount()
	if err != nil {
		return err
	}
	account := rawAccount.(*InternalAccount)

	nowSec := time.Now().Unix()

	entry := &idEntry{
		id:             account.ID,
		userIdx:        idx,
		lastSec:        protocol.Timestamp(nowSec - cacheDurationSec),
		lastSecRemoval: protocol.Timestamp(nowSec - cacheDurationSec*3),
	}
	this.generateNewHashes(protocol.Timestamp(nowSec+cacheDurationSec), idx, entry)
	this.ids = append(this.ids, entry)
	for _, alterid := range account.AlterIDs {
		entry := &idEntry{
			id:             alterid,
			userIdx:        idx,
			lastSec:        protocol.Timestamp(nowSec - cacheDurationSec),
			lastSecRemoval: protocol.Timestamp(nowSec - cacheDurationSec*3),
		}
		this.generateNewHashes(protocol.Timestamp(nowSec+cacheDurationSec), idx, entry)
		this.ids = append(this.ids, entry)
	}

	return nil
}

func (this *TimedUserValidator) Get(userHash []byte) (*protocol.User, protocol.Timestamp, bool) {
	defer this.RUnlock()
	this.RLock()

	if !this.running {
		return nil, 0, false
	}
	var fixedSizeHash [16]byte
	copy(fixedSizeHash[:], userHash)
	pair, found := this.userHash[fixedSizeHash]
	if found {
		return this.validUsers[pair.index], pair.timeSec, true
	}
	return nil, 0, false
}
