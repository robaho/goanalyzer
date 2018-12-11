// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package trace

import (
	"sort"
)

// GDesc contains statistics and execution details of a single goroutine.
type GDesc struct {
	ID           uint64
	Name         string
	PC           uint64
	CreationTime int64
	StartTime    int64
	EndTime      int64

	// List of regions in the goroutine, sorted based on the start time.
	Regions []*UserRegionDesc

	// Statistics of execution time during the goroutine execution.
	GExecutionStat

	*gdesc // private part.
}

// UserRegionDesc represents a region and goroutine execution stats
// while the region was active.
type UserRegionDesc struct {
	TaskID uint64
	Name   string

	// Region start event. Normally EvUserRegion start event or nil,
	// but can be EvGoCreate event if the region is a synthetic
	// region representing task inheritance from the parent goroutine.
	Start *Event

	// Region end event. Normally EvUserRegion end event or nil,
	// but can be EvGoStop or EvGoEnd event if the goroutine
	// terminated without explicitly ending the region.
	End *Event

	GExecutionStat
}

type GExecutionStatEntry struct {
	Count int64
	Total int64
	Min   int64
	Max   int64
}

func (s *GExecutionStatEntry) addTime(time int64) {
	if time == 0 { // not possible
		return
	}
	if s.Count == 0 {
		s.Min = time
		s.Max = time
	}
	s.Count++
	s.Total += time
	if time < s.Min {
		s.Min = time
	}
	if time > s.Max {
		s.Max = time
	}
}

func (s *GExecutionStatEntry) AddStat(s2 GExecutionStatEntry) {
	if s.Count == 0 {
		s.Min = s2.Min
		s.Max = s2.Max
	} else if s2.Count > 0 {
		if s2.Min < s.Min {
			s.Min = s2.Min
		}
		if s2.Max > s.Max {
			s.Max = s2.Max
		}
	}
	s.Total += s2.Total
	s.Count += s2.Count
}

// GExecutionStat contains statistics about a goroutine's execution
// during a period of time.
type GExecutionStat struct {
	ExecTime      GExecutionStatEntry
	SchedWaitTime GExecutionStatEntry
	IOTime        GExecutionStatEntry
	BlockTime     GExecutionStatEntry
	SyscallTime   GExecutionStatEntry
	GCTime        GExecutionStatEntry
	SweepTime     GExecutionStatEntry
	TotalTime     GExecutionStatEntry
}

// sub returns the stats v-s.
func (s GExecutionStat) sub(v GExecutionStat) (r GExecutionStat) {
	r = s
	r.ExecTime.Total -= v.ExecTime.Total
	r.SchedWaitTime.Total -= v.SchedWaitTime.Total
	r.IOTime.Total -= v.IOTime.Total
	r.BlockTime.Total -= v.BlockTime.Total
	r.SyscallTime.Total -= v.SyscallTime.Total
	r.GCTime.Total -= v.GCTime.Total
	r.SweepTime.Total -= v.SweepTime.Total
	r.TotalTime.Total -= v.TotalTime.Total
	return r
}

// snapshotStat returns the snapshot of the goroutine execution statistics.
// This is called as we process the ordered trace event stream. lastTs and
// activeGCStartTime are used to process pending statistics if this is called
// before any goroutine end event.
func (g *GDesc) snapshotStat(lastTs, activeGCStartTime int64) (ret GExecutionStat) {
	ret = g.GExecutionStat

	if g.gdesc == nil {
		return ret // finalized GDesc. No pending state.
	}

	if activeGCStartTime != 0 { // terminating while GC is active
		if g.CreationTime < activeGCStartTime {
			ret.GCTime.addTime(lastTs - activeGCStartTime)
		} else {
			// The goroutine's lifetime completely overlaps
			// with a GC.
			ret.GCTime.addTime(lastTs - g.CreationTime)
		}
	}

	if g.TotalTime.Count == 0 {
		ret.TotalTime.addTime(lastTs - g.CreationTime)
	}

	if g.lastStartTime != 0 {
		ret.ExecTime.addTime(lastTs - g.lastStartTime)
	}
	if g.blockNetTime != 0 {
		ret.IOTime.addTime(lastTs - g.blockNetTime)
	}
	if g.blockSyncTime != 0 {
		ret.BlockTime.addTime(lastTs - g.blockSyncTime)
	}
	if g.blockSyscallTime != 0 {
		ret.SyscallTime.addTime(lastTs - g.blockSyscallTime)
	}
	if g.blockSchedTime != 0 {
		ret.SchedWaitTime.addTime(lastTs - g.blockSchedTime)
	}
	if g.blockSweepTime != 0 {
		ret.SweepTime.addTime(lastTs - g.blockSweepTime)
	}
	return ret
}

// finalize is called when processing a goroutine end event or at
// the end of trace processing. This finalizes the execution GExecutionStatEntry
// and any active regions in the goroutine, in which case trigger is nil.
func (g *GDesc) finalize(lastTs, activeGCStartTime int64, trigger *Event) {
	if trigger != nil {
		g.EndTime = trigger.Ts
	}
	finalStat := g.snapshotStat(lastTs, activeGCStartTime)

	g.GExecutionStat = finalStat
	for _, s := range g.activeRegions {
		s.End = trigger
		s.GExecutionStat = finalStat.sub(s.GExecutionStat)
		g.Regions = append(g.Regions, s)
	}
	*(g.gdesc) = gdesc{}
}

// gdesc is a private part of GDesc that is required only during analysis.
type gdesc struct {
	lastStartTime    int64
	blockNetTime     int64
	blockSyncTime    int64
	blockSyscallTime int64
	blockSweepTime   int64
	blockGCTime      int64
	blockSchedTime   int64

	activeRegions []*UserRegionDesc // stack of active regions
}

// GoroutineStats generates statistics for all goroutines in the trace.
func GoroutineStats(events []*Event) map[uint64]*GDesc {
	gs := make(map[uint64]*GDesc)
	var lastTs int64
	var gcStartTime int64 // gcStartTime == 0 indicates gc is inactive.
	for _, ev := range events {
		lastTs = ev.Ts
		switch ev.Type {
		case EvGoCreate:
			g := &GDesc{ID: ev.Args[0], CreationTime: ev.Ts, gdesc: new(gdesc)}
			g.blockSchedTime = ev.Ts
			// When a goroutine is newly created, inherit the
			// task of the active region. For ease handling of
			// this case, we create a fake region description with
			// the task id.
			if creatorG := gs[ev.G]; creatorG != nil && len(creatorG.gdesc.activeRegions) > 0 {
				regions := creatorG.gdesc.activeRegions
				s := regions[len(regions)-1]
				if s.TaskID != 0 {
					g.gdesc.activeRegions = []*UserRegionDesc{
						{TaskID: s.TaskID, Start: ev},
					}
				}
			}
			gs[g.ID] = g
		case EvGoStart, EvGoStartLabel:
			g := gs[ev.G]
			if g.PC == 0 {
				g.PC = ev.Stk[0].PC
				g.Name = ev.Stk[0].Fn
			}
			g.lastStartTime = ev.Ts
			if g.StartTime == 0 {
				g.StartTime = ev.Ts
			}
			if g.blockSchedTime != 0 {
				g.SchedWaitTime.addTime(ev.Ts - g.blockSchedTime)
				g.blockSchedTime = 0
			}
		case EvGoEnd, EvGoStop:
			g := gs[ev.G]
			g.finalize(ev.Ts, gcStartTime, ev)
		case EvGoBlockSend, EvGoBlockRecv, EvGoBlockSelect,
			EvGoBlockSync, EvGoBlockCond:
			g := gs[ev.G]
			g.ExecTime.addTime(ev.Ts - g.lastStartTime)
			g.lastStartTime = 0
			g.blockSyncTime = ev.Ts
		case EvGoSched, EvGoPreempt:
			g := gs[ev.G]
			g.ExecTime.addTime(ev.Ts - g.lastStartTime)
			g.lastStartTime = 0
			g.blockSchedTime = ev.Ts
		case EvGoSleep, EvGoBlock:
			g := gs[ev.G]
			g.ExecTime.addTime(ev.Ts - g.lastStartTime)
			g.lastStartTime = 0
		case EvGoBlockNet:
			g := gs[ev.G]
			g.ExecTime.addTime(ev.Ts - g.lastStartTime)
			g.lastStartTime = 0
			g.blockNetTime = ev.Ts
		case EvGoBlockGC:
			g := gs[ev.G]
			g.ExecTime.addTime(ev.Ts - g.lastStartTime)
			g.lastStartTime = 0
			g.blockGCTime = ev.Ts
		case EvGoUnblock:
			g := gs[ev.Args[0]]
			if g.blockNetTime != 0 {
				g.IOTime.addTime(ev.Ts - g.blockNetTime)
				g.blockNetTime = 0
			}
			if g.blockSyncTime != 0 {
				g.BlockTime.addTime(ev.Ts - g.blockSyncTime)
				g.blockSyncTime = 0
			}
			g.blockSchedTime = ev.Ts
		case EvGoSysBlock:
			g := gs[ev.G]
			g.ExecTime.addTime(ev.Ts - g.lastStartTime)
			g.lastStartTime = 0
			g.blockSyscallTime = ev.Ts
		case EvGoSysExit:
			g := gs[ev.G]
			if g.blockSyscallTime != 0 {
				g.SyscallTime.addTime(ev.Ts - g.blockSyscallTime)
				g.blockSyscallTime = 0
			}
			g.blockSchedTime = ev.Ts
		case EvGCSweepStart:
			g := gs[ev.G]
			if g != nil {
				// Sweep can happen during GC on system goroutine.
				g.blockSweepTime = ev.Ts
			}
		case EvGCSweepDone:
			g := gs[ev.G]
			if g != nil && g.blockSweepTime != 0 {
				g.SweepTime.addTime(ev.Ts - g.blockSweepTime)
				g.blockSweepTime = 0
			}
		case EvGCStart:
			gcStartTime = ev.Ts
		case EvGCDone:
			for _, g := range gs {
				if g.EndTime != 0 {
					continue
				}
				if gcStartTime < g.CreationTime {
					g.GCTime.addTime(ev.Ts - g.CreationTime)
				} else {
					g.GCTime.addTime(ev.Ts - gcStartTime)
				}
			}
			gcStartTime = 0 // indicates gc is inactive.
		case EvUserRegion:
			g := gs[ev.G]
			switch mode := ev.Args[1]; mode {
			case 0: // region start
				g.activeRegions = append(g.activeRegions, &UserRegionDesc{
					Name:           ev.SArgs[0],
					TaskID:         ev.Args[0],
					Start:          ev,
					GExecutionStat: g.snapshotStat(lastTs, gcStartTime),
				})
			case 1: // region end
				var sd *UserRegionDesc
				if regionStk := g.activeRegions; len(regionStk) > 0 {
					n := len(regionStk)
					sd = regionStk[n-1]
					regionStk = regionStk[:n-1] // pop
					g.activeRegions = regionStk
				} else {
					sd = &UserRegionDesc{
						Name:   ev.SArgs[0],
						TaskID: ev.Args[0],
					}
				}
				sd.GExecutionStat = g.snapshotStat(lastTs, gcStartTime).sub(sd.GExecutionStat)
				sd.End = ev
				g.Regions = append(g.Regions, sd)
			}
		}
	}

	for _, g := range gs {
		g.finalize(lastTs, gcStartTime, nil)

		// sort based on region start time
		sort.Slice(g.Regions, func(i, j int) bool {
			x := g.Regions[i].Start
			y := g.Regions[j].Start
			if x == nil {
				return true
			}
			if y == nil {
				return false
			}
			return x.Ts < y.Ts
		})

		g.gdesc = nil
	}

	return gs
}

// RelatedGoroutines finds a set of goroutines related to goroutine goid.
func RelatedGoroutines(events []*Event, goid uint64) map[uint64]bool {
	// BFS of depth 2 over "unblock" edges
	// (what goroutines unblock goroutine goid?).
	gmap := make(map[uint64]bool)
	gmap[goid] = true
	for i := 0; i < 2; i++ {
		gmap1 := make(map[uint64]bool)
		for g := range gmap {
			gmap1[g] = true
		}
		for _, ev := range events {
			if ev.Type == EvGoUnblock && gmap[ev.Args[0]] {
				gmap1[ev.G] = true
			}
		}
		gmap = gmap1
	}
	gmap[0] = true // for GC events
	return gmap
}
