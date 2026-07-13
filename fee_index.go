package main

import (
	"sync"
)

// feeIndexKey is the lookup key for the in-memory fee schedule index.
type feeIndexKey struct {
	tier  string
	asset string
	side  string
}

// feeIndexEntry holds all enabled schedules for a (tier, asset, side) triple,
// sorted by size band for a linear scan over a small slice. O(1) lookup of the
// candidate list; the size-band match is O(k) where k is small (typically 2-4).
type feeIndex struct {
	mu   sync.RWMutex
	data map[feeIndexKey][]FeeSchedule
}

func newFeeIndex() *feeIndex {
	return &feeIndex{data: make(map[feeIndexKey][]FeeSchedule)}
}

// Rebuild replaces the index with the given schedules.
func (f *feeIndex) Rebuild(schedules []FeeSchedule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = make(map[feeIndexKey][]FeeSchedule, len(schedules))
	for _, s := range schedules {
		if !s.Enabled {
			continue
		}
		k := feeIndexKey{tier: s.UserTier, asset: s.Asset, side: s.Side}
		f.data[k] = append(f.data[k], s)
	}
}

// Lookup returns the first enabled schedule matching the (tier, asset, side)
// and the amount's size band, or nil if none matches.
func (f *feeIndex) Lookup(tier, asset, side string, amount float64) *FeeSchedule {
	f.mu.RLock()
	defer f.mu.RUnlock()
	cands := f.data[feeIndexKey{tier: tier, asset: asset, side: side}]
	for i := range cands {
		s := cands[i]
		if amount < s.SizeBandMin {
			continue
		}
		if s.SizeBandMax > 0 && amount > s.SizeBandMax {
			continue
		}
		return &cands[i]
	}
	return nil
}

// Len returns the number of enabled indexed schedules.
func (f *feeIndex) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n := 0
	for _, v := range f.data {
		n += len(v)
	}
	return n
}