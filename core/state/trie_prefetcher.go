// (c) 2019-2020, Ava Labs, Inc.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"sync"

	"github.com/ava-labs/coreth/metrics"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

var (
	// triePrefetchMetricsPrefix is the prefix under which to publish the metrics.
	triePrefetchMetricsPrefix = "trie/prefetch/"
)

// triePrefetcher is an active prefetcher, which receives accounts or storage
// items and does trie-loading of them. The goal is to get as much useful content
// into the caches as possible.
//
// Note, the prefetcher's API is not thread safe.
type triePrefetcher struct {
	db       Database               // Database to fetch trie nodes through
	root     common.Hash            // Root hash of the account trie for metrics
	fetches  map[string]Trie        // Partially or fully fetcher tries
	fetchers map[string]*subfetcher // Subfetchers for each trie

	deliveryCopyMissMeter    metrics.Meter
	deliveryRequestMissMeter metrics.Meter
	deliveryWaitMissMeter    metrics.Meter

	accountLoadMeter  metrics.Meter
	accountDupMeter   metrics.Meter
	accountSkipMeter  metrics.Meter
	accountWasteMeter metrics.Meter
	storageLoadMeter  metrics.Meter
	storageDupMeter   metrics.Meter
	storageSkipMeter  metrics.Meter
	storageWasteMeter metrics.Meter
}

func newTriePrefetcher(db Database, root common.Hash, namespace string) *triePrefetcher {
	prefix := triePrefetchMetricsPrefix + namespace
	p := &triePrefetcher{
		db:       db,
		root:     root,
		fetchers: make(map[string]*subfetcher), // Active prefetchers use the fetchers map

		deliveryCopyMissMeter:    metrics.GetOrRegisterMeter(prefix+"/deliverymiss/copy", nil),
		deliveryRequestMissMeter: metrics.GetOrRegisterMeter(prefix+"/deliverymiss/request", nil),
		deliveryWaitMissMeter:    metrics.GetOrRegisterMeter(prefix+"/deliverymiss/wait", nil),

		accountLoadMeter:  metrics.GetOrRegisterMeter(prefix+"/account/load", nil),
		accountDupMeter:   metrics.GetOrRegisterMeter(prefix+"/account/dup", nil),
		accountSkipMeter:  metrics.GetOrRegisterMeter(prefix+"/account/skip", nil),
		accountWasteMeter: metrics.GetOrRegisterMeter(prefix+"/account/waste", nil),
		storageLoadMeter:  metrics.GetOrRegisterMeter(prefix+"/storage/load", nil),
		storageDupMeter:   metrics.GetOrRegisterMeter(prefix+"/storage/dup", nil),
		storageSkipMeter:  metrics.GetOrRegisterMeter(prefix+"/storage/skip", nil),
		storageWasteMeter: metrics.GetOrRegisterMeter(prefix+"/storage/waste", nil),
	}
	return p
}

// close iterates over all the subfetchers, aborts any that were left spinning
// and reports the stats to the metrics subsystem.
func (p *triePrefetcher) close() {
	for _, fetcher := range p.fetchers {
		fetcher.abort() // safe to do multiple times

		if metrics.Enabled {
			if fetcher.root == p.root {
				p.accountLoadMeter.Mark(int64(len(fetcher.seen)))
				p.accountDupMeter.Mark(int64(fetcher.dups))
				p.accountSkipMeter.Mark(int64(len(fetcher.tasks)))

				for _, key := range fetcher.used {
					delete(fetcher.seen, string(key))
				}
				p.accountWasteMeter.Mark(int64(len(fetcher.seen)))
			} else {
				p.storageLoadMeter.Mark(int64(len(fetcher.seen)))
				p.storageDupMeter.Mark(int64(fetcher.dups))
				p.storageSkipMeter.Mark(int64(len(fetcher.tasks)))

				for _, key := range fetcher.used {
					delete(fetcher.seen, string(key))
				}
				p.storageWasteMeter.Mark(int64(len(fetcher.seen)))
			}
		}
	}
	// Clear out all fetchers (will crash on a second call, deliberate)
	p.fetchers = nil
}

// copy creates a deep-but-inactive copy of the trie prefetcher. Any trie data
// already loaded will be copied over, but no goroutines will be started. This
// is mostly used in the miner which creates a copy of it's actively mutated
// state to be sealed while it may further mutate the state.
func (p *triePrefetcher) copy() *triePrefetcher {
	copy := &triePrefetcher{
		db:      p.db,
		root:    p.root,
		fetches: make(map[string]Trie), // Active prefetchers use the fetchers map

		deliveryCopyMissMeter:    p.deliveryCopyMissMeter,
		deliveryRequestMissMeter: p.deliveryRequestMissMeter,
		deliveryWaitMissMeter:    p.deliveryWaitMissMeter,

		accountLoadMeter:  p.accountLoadMeter,
		accountDupMeter:   p.accountDupMeter,
		accountSkipMeter:  p.accountSkipMeter,
		accountWasteMeter: p.accountWasteMeter,
		storageLoadMeter:  p.storageLoadMeter,
		storageDupMeter:   p.storageDupMeter,
		storageSkipMeter:  p.storageSkipMeter,
		storageWasteMeter: p.storageWasteMeter,
	}
	// If the prefetcher is already a copy, duplicate the data
	if p.fetches != nil {
		for root, fetch := range p.fetches {
			if fetch == nil {
				continue
			}
			copy.fetches[root] = p.db.CopyTrie(fetch)
		}
		return copy
	}
	// Otherwise we're copying an active fetcher, retrieve the current states
	for id, fetcher := range p.fetchers {
		// TODO: need to support handing over WIP copy?
		copy.fetches[id] = fetcher.peek()
	}
	return copy
}

// prefetch schedules a batch of trie items to prefetch.
func (p *triePrefetcher) prefetch(owner common.Hash, root common.Hash, addr common.Address, keys [][]byte) {
	// If the prefetcher is an inactive one, bail out
	if p.fetches != nil {
		return
	}
	// Active fetcher, schedule the retrievals
	id := p.trieID(owner, root)
	fetcher := p.fetchers[id]
	if fetcher == nil {
		// TODO: limit number of subFetchers
		fetcher = newSubfetcher(p.db, p.root, owner, root, addr)
		p.fetchers[id] = fetcher
	}
	fetcher.schedule(keys)
}

// trie returns the trie matching the root hash, or nil if the prefetcher doesn't
// have it.
func (p *triePrefetcher) trie(owner common.Hash, root common.Hash) Trie {
	// If the prefetcher is inactive, return from existing deep copies
	id := p.trieID(owner, root)
	if p.fetches != nil {
		trie := p.fetches[id]
		if trie == nil {
			p.deliveryCopyMissMeter.Mark(1)
			return nil
		}
		return p.db.CopyTrie(trie)
	}
	// Otherwise the prefetcher is active, bail if no trie was prefetched for this root
	fetcher := p.fetchers[id]
	if fetcher == nil {
		p.deliveryRequestMissMeter.Mark(1)
		return nil
	}

	// Wait for the fetcher to finish (or continue if it exited unexpectedly).
	select {
	case <-fetcher.finished:
		// Stop the prefetcher when it is done.
		//
		// [abort] is safe to call multiple times.
		fetcher.abort()
	case <-fetcher.term:
	}

	// Return a copy of one of the prefetched tries (this is still backed
	// by a node cache).
	trie := fetcher.peek()
	if trie == nil {
		p.deliveryWaitMissMeter.Mark(1)
		return nil
	}
	return trie
}

// used marks a batch of state items used to allow creating statistics as to
// how useful or wasteful the prefetcher is.
func (p *triePrefetcher) used(owner common.Hash, root common.Hash, used [][]byte) {
	if fetcher := p.fetchers[p.trieID(owner, root)]; fetcher != nil {
		fetcher.used = used
	}
}

// trieID returns an unique trie identifier consists the trie owner and root hash.
func (p *triePrefetcher) trieID(owner common.Hash, root common.Hash) string {
	return string(append(owner.Bytes(), root.Bytes()...))
}

// subfetcher is a trie fetcher goroutine responsible for pulling entries for a
// single trie. It is spawned when a new root is encountered and lives until the
// main prefetcher is paused and either all requested items are processed or if
// the trie being worked on is retrieved from the prefetcher.
type subfetcher struct {
	db    Database       // Database to load trie nodes through
	state common.Hash    // Root hash of the state to prefetch
	owner common.Hash    // Owner of the trie, usually account hash
	root  common.Hash    // Root hash of the trie to prefetch
	addr  common.Address // Address of the account that the trie belongs to
	trie  Trie           // Trie being populated with nodes

	tasks [][]byte   // Items queued up for retrieval
	lock  sync.Mutex // Lock protecting the task queue

	wake         chan struct{} // Wake channel if a new task is scheduled
	stop         chan struct{} // Channel to interrupt processing
	term         chan struct{} // Channel to signal interruption
	finished     chan struct{} // Channel to signal prefetching is done
	finishedOnce sync.Once
	copy         chan chan Trie // Channel to request a copy of the current trie

	seen map[string]struct{} // Tracks the entries already loaded
	dups int                 // Number of duplicate preload tasks
	used [][]byte            // Tracks the entries used in the end
}

// newSubfetcher creates a goroutine to prefetch state items belonging to a
// particular root hash.
func newSubfetcher(db Database, state common.Hash, owner common.Hash, root common.Hash, addr common.Address) *subfetcher {
	sf := &subfetcher{
		db:       db,
		state:    state,
		owner:    owner,
		root:     root,
		addr:     addr,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		term:     make(chan struct{}),
		finished: make(chan struct{}),
		copy:     make(chan chan Trie),
		seen:     make(map[string]struct{}),
	}
	go sf.loop()
	return sf
}

// schedule adds a batch of trie keys to the queue to prefetch.
func (sf *subfetcher) schedule(keys [][]byte) {
	// TODO: limit total live prefetching across all items in all prefetchers

	// Append the tasks to the current queue
	sf.lock.Lock()
	for _, key := range keys {
		// Check if keys already seen
		sk := string(key)
		if _, ok := sf.seen[sk]; ok {
			sf.dups++
			continue
		}
		sf.seen[sk] = struct{}{}
		sf.tasks = append(sf.tasks, key)
	}
	sf.lock.Unlock()

	// Notify the prefetcher, it's fine if it's already terminated
	select {
	case sf.wake <- struct{}{}:
	default:
	}
}

// peek tries to retrieve a deep copy of the fetcher's trie in whatever form it
// is currently.
func (sf *subfetcher) peek() Trie {
	ch := make(chan Trie)
	select {
	case sf.copy <- ch:
		// Subfetcher still alive, return copy from it
		return <-ch

	case <-sf.term:
		// Subfetcher already terminated, return a copy directly
		if sf.trie == nil {
			return nil
		}
		// TODO: find a way to merge all tries we prefetched instead of returning the first one
		return sf.db.CopyTrie(sf.trie)
	}
}

// abort interrupts the subfetcher immediately. It is safe to call abort multiple
// times but it is not thread safe.
func (sf *subfetcher) abort() {
	select {
	case <-sf.stop:
	default:
		close(sf.stop)
	}
	<-sf.term
}

// loop waits for new tasks to be scheduled and keeps loading them until it runs
// out of tasks or its underlying trie is retrieved for committing.
func (sf *subfetcher) loop() {
	// No matter how the loop stops, signal anyone waiting that it's terminated
	defer close(sf.term)

	// Start by opening the trie and stop processing if it fails
	if sf.owner == (common.Hash{}) {
		trie, err := sf.db.OpenTrie(sf.root)
		if err != nil {
			log.Warn("Trie prefetcher failed opening trie", "root", sf.root, "err", err)
			return
		}
		sf.trie = trie
	} else {
		trie, err := sf.db.OpenStorageTrie(sf.state, sf.owner, sf.root)
		if err != nil {
			log.Warn("Trie prefetcher failed opening trie", "root", sf.root, "err", err)
			return
		}
		sf.trie = trie
	}

	// Trie opened successfully, keep prefetching items
	for {
		select {
		case <-sf.wake:
			// Subfetcher was woken up, retrieve any tasks to avoid spinning the lock
			sf.lock.Lock()
			tasks := sf.tasks
			sf.tasks = nil
			sf.lock.Unlock()

			// Prefetch any tasks until the loop is interrupted
			for i, task := range tasks {
				select {
				case <-sf.stop:
					// TODO: wait for trie to finish or exit with failure (will undoubtedly be faster than passing over to iterate 1-by-1)?
					//
					// If termination is requested, add any leftover back and return
					sf.lock.Lock()
					sf.tasks = append(sf.tasks, tasks[i:]...)
					sf.lock.Unlock()
					return

				case ch := <-sf.copy:
					// Somebody wants a copy of the current trie, grant them
					//
					// TODO: this should never happen during trie generation
					ch <- sf.db.CopyTrie(sf.trie)

				default:
					// No termination request yet, prefetch the next entry
					//
					// TODO: save trie in each goroutine that is run concurrently rather than
					// creating a new one for each key.
					var err error
					if len(task) == common.AddressLength {
						_, err = sf.trie.GetAccount(common.BytesToAddress(task))
					} else {
						_, err = sf.trie.GetStorage(sf.addr, task)
					}
					if err != nil {
						log.Error("Trie prefetcher failed fetching", "root", sf.root, "err", err)
					}
				}
			}

			// Fetch finished
			sf.finishedOnce.Do(func() {
				close(sf.finished)
			})
		case ch := <-sf.copy:
			// Somebody wants a copy of the current trie, grant them
			ch <- sf.db.CopyTrie(sf.trie)

		case <-sf.stop:
			// Termination is requested, abort and leave remaining tasks
			return
		}
	}
}
