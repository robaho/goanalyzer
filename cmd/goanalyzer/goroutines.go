// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Goroutine-related profiles.

package main

import (
	"fmt"
	"github.com/robaho/goanalyzer/cmd/goanalyzer/internal/trace"
	"html/template"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"
)

func init() {
	http.HandleFunc("/goroutines", httpGoroutines)
	http.HandleFunc("/goroutine", httpGoroutine)
}

// gtype describes a group of goroutines grouped by start PC.
type gtype struct {
	ID   uint64 // Unique identifier (PC).
	Name string // Start function.
	N    int    // Total number of goroutines in this group.
	trace.GExecutionStat
}

var (
	gsInit sync.Once
	gs     map[uint64]*trace.GDesc
)

// analyzeGoroutines generates statistics about execution of all goroutines and stores them in gs.
func analyzeGoroutines(events []*trace.Event) {
	gsInit.Do(func() {
		gs = trace.GoroutineStats(events)
	})
}

// httpGoroutines serves list of goroutine groups.
func httpGoroutines(w http.ResponseWriter, r *http.Request) {
	events, err := parseEvents()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	analyzeGoroutines(events)
	gss := make(map[uint64]gtype)

	var totalExecTime int64
	var maxTotalTime int64
	var n int64

	for _, g := range gs {
		gs1 := gss[g.PC]
		gs1.ID = g.PC
		gs1.Name = g.Name

		if gs1.Name == "" {
			gs1.Name = fmt.Sprint("PC:", g.PC)
		}

		gs1.N++
		gs1.ExecTime.AddStat(g.ExecTime)
		gs1.BlockTime.AddStat(g.BlockTime)
		gs1.GCTime.AddStat(g.GCTime)
		gs1.IOTime.AddStat(g.IOTime)
		gs1.SchedWaitTime.AddStat(g.SchedWaitTime)
		gs1.SyscallTime.AddStat(g.SyscallTime)
		gs1.TotalTime.AddStat(g.TotalTime)

		totalExecTime += g.ExecTime.Total

		if maxTotalTime < gs1.TotalTime.Total {
			maxTotalTime = gs1.TotalTime.Total
		}

		gss[g.PC] = gs1
	}
	var glist []gtype
	for k, v := range gss {
		v.ID = k
		glist = append(glist, v)
	}

	sortby := r.FormValue("sortby")
	_, ok := reflect.TypeOf(gtype{}).FieldByNameFunc(func(s string) bool {
		return s == sortby
	})
	if !ok {
		sortby = "ExecTime"
	}

	sort.SliceStable(glist, func(i, j int) bool {
		ival := reflect.ValueOf(glist[i]).FieldByName(sortby).FieldByName("Total").Int()
		jval := reflect.ValueOf(glist[j]).FieldByName(sortby).FieldByName("Total").Int()
		if ival == jval {
			return glist[i].ID > glist[j].ID
		}
		return ival > jval
	})

	w.Header().Set("Content-Type", "text/html;charset=utf-8")

	err = templGoroutines.Execute(w, struct {
		N             int64
		TotalExecTime int64
		GList         []gtype
	}{
		N:             n,
		TotalExecTime: totalExecTime,
		GList:         glist})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to execute template: %v", err), http.StatusInternalServerError)
		return
	}
}

var templGoroutines = template.Must(template.New("").Funcs(template.FuncMap{
	"prettyDuration": func(s trace.GExecutionStatEntry) template.HTML {
		d := time.Duration(s.Total) * time.Nanosecond
		return template.HTML(niceDuration(d))
	},
	"percent": func(dividened, divisor int64) template.HTML {
		if divisor == 0 {
			return ""
		}
		return template.HTML(fmt.Sprintf("(%.1f%%)", float64(dividened)/float64(divisor)*100))
	},
	"minavgmax": func(s trace.GExecutionStatEntry) template.HTML {
		if s.Count == 0 {
			return ""
		}
		d := time.Duration(s.Total/s.Count) * time.Nanosecond
		min := time.Duration(s.Min) * time.Nanosecond
		max := time.Duration(s.Max) * time.Nanosecond
		return "<small>" + template.HTML("["+niceDuration(min)+"/"+niceDuration(d)+"/"+niceDuration(max)+"]") + "</small>"
	},
	"barLen": func(s trace.GExecutionStatEntry, total trace.GExecutionStatEntry) template.HTML {
		if total.Total == 0 {
			return "0"
		}
		return template.HTML(fmt.Sprintf("%.2f%%", float64(s.Total)/float64(total.Total)*100))
	},
	"unknownTime": func(desc *trace.GExecutionStat) trace.GExecutionStatEntry {
		sum := desc.ExecTime.Total + desc.IOTime.Total + desc.BlockTime.Total + desc.SyscallTime.Total + desc.SchedWaitTime.Total
		if sum < desc.TotalTime.Total {
			e := trace.GExecutionStatEntry{}
			e.Total = desc.TotalTime.Total - sum
			return e
		}
		return trace.GExecutionStatEntry{}
	},
}).Parse(`
<html>
<style>
th {
  background-color: #050505;
  color: #fff;
}
table {
  border-collapse: collapse;
}
.details tr:hover {
  background-color: #f2f2f2;
}
.details td {
  text-align: right;
  border: 1px solid black;
}
.details td.id {
  text-align: left;
}
.stacked-bar-graph {
  width: 300px;
  height: 10px;
  color: #414042;
  white-space: nowrap;
  font-size: 5px;
}
.stacked-bar-graph span {
  display: inline-block;
  width: 100%;
  height: 100%;
  box-sizing: border-box;
  float: left;
  padding: 0;
}
.unknown-time { background-color: #636363; }
.exec-time { background-color: #d7191c; }
.io-time { background-color: #fdae61; }
.block-time { background-color: #d01c8b; }
.syscall-time { background-color: #7b3294; }
.sched-time { background-color: #2c7bb6; }
</style>
<script>
function reloadTable(key, value) {
  let params = new URLSearchParams(window.location.search);
  params.set(key, value);
  window.location.search = params.toString();
}
</script>
<body>
<table class="details">
<tr>
<th> Goroutine</th>
<th onclick="reloadTable('sortby', 'N')"> Count</th>
<th onclick="reloadTable('sortby', 'TotalTime')"> Total</th>
<th></th>
<th onclick="reloadTable('sortby', 'ExecTime')" class="exec-time"> Execution</th>
<th onclick="reloadTable('sortby', 'IOTime')" class="io-time"> Network wait</th>
<th onclick="reloadTable('sortby', 'BlockTime')" class="block-time"> Sync block </th>
<th onclick="reloadTable('sortby', 'SyscallTime')" class="syscall-time"> Blocking syscall</th>
<th onclick="reloadTable('sortby', 'SchedWaitTime')" class="sched-time"> Scheduler wait</th>
<th onclick="reloadTable('sortby', 'SweepTime')"> GC sweeping</th>
<th onclick="reloadTable('sortby', 'GCTime')"> GC pause</th>
</tr>
{{range $i,$e := .GList}}
  <tr>
	<td><a href="/goroutine?id={{.ID}}">{{.Name}}</a></td>
	<td>{{.N}}</td>
	{{with .GExecutionStat}}
    <td> {{prettyDuration .TotalTime}} </td>
    <td>
	<div class="stacked-bar-graph">
	  {{if unknownTime .}}<span style="width:{{barLen (unknownTime .) .TotalTime}}" class="unknown-time">&nbsp;</span>{{end}}
          {{if .ExecTime.Count}}<span style="width:{{barLen .ExecTime .TotalTime}}" class="exec-time">&nbsp;</span>{{end}}
          {{if .IOTime.Count}}<span style="width:{{barLen .IOTime .TotalTime}}" class="io-time">&nbsp;</span>{{end}}
          {{if .BlockTime.Count}}<span style="width:{{barLen .BlockTime .TotalTime}}" class="block-time">&nbsp;</span>{{end}}
          {{if .SyscallTime.Count}}<span style="width:{{barLen .SyscallTime .TotalTime}}" class="syscall-time">&nbsp;</span>{{end}}
          {{if .SchedWaitTime.Count}}<span style="width:{{barLen .SchedWaitTime .TotalTime}}" class="sched-time">&nbsp;</span>{{end}}
        </div>
    </td>
    <td> {{prettyDuration .ExecTime}}  {{percent .ExecTime.Total $.TotalExecTime}} {{minavgmax .ExecTime}}</td>
    <td><a href="/io?id={{$e.ID}}"> {{prettyDuration .IOTime}} {{minavgmax .IOTime}}</a></td>
    <td><a href="/block?id={{$e.ID}}"> {{prettyDuration .BlockTime}} {{minavgmax .BlockTime}}</a></td>
    <td><a href="/syscall?id={{$e.ID}}"> {{prettyDuration .SyscallTime}} {{minavgmax .SyscallTime}}</a></td>
    <td><a href="/sched?id={{$e.ID}}"> {{prettyDuration .SchedWaitTime}} {{minavgmax .SchedWaitTime}}</a></td>
    <td> {{prettyDuration .SweepTime}} {{percent .SweepTime.Total .TotalTime.Total}}</td>
    <td> {{prettyDuration .GCTime}} {{percent .GCTime.Total .TotalTime.Total}} {{minavgmax .GCTime}}</td>
	{{end}}
  </tr>
{{end}}
</table>
</body>
</html>
`))

// httpGoroutine serves list of goroutines in a particular group.
func httpGoroutine(w http.ResponseWriter, r *http.Request) {
	// TODO(hyangah): support format=csv (raw data)

	events, err := parseEvents()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pc, err := strconv.ParseUint(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to parse id parameter '%v': %v", r.FormValue("id"), err), http.StatusInternalServerError)
		return
	}
	analyzeGoroutines(events)
	var (
		glist                   []*trace.GDesc
		name                    string
		totalExecTime, execTime int64
		maxTotalTime            int64
	)

	for _, g := range gs {
		totalExecTime += g.ExecTime.Total

		if g.PC != pc {
			continue
		}
		glist = append(glist, g)
		name = g.Name
		execTime += g.ExecTime.Total
		if maxTotalTime < g.TotalTime.Total {
			maxTotalTime = g.TotalTime.Total
		}
	}

	execTimePercent := ""
	if totalExecTime > 0 {
		execTimePercent = fmt.Sprintf("%.2f%%", float64(execTime)/float64(totalExecTime)*100)
	}

	sortby := r.FormValue("sortby")
	_, ok := reflect.TypeOf(trace.GDesc{}).FieldByNameFunc(func(s string) bool {
		return s == sortby
	})
	if !ok {
		sortby = "ExecTime"
	}

	sort.Slice(glist, func(i, j int) bool {
		ival := reflect.ValueOf(glist[i]).Elem().FieldByName(sortby).FieldByName("Total").Int()
		jval := reflect.ValueOf(glist[j]).Elem().FieldByName(sortby).FieldByName("Total").Int()
		if ival == jval {
			return glist[i].ID > glist[j].ID
		}
		return ival > jval
	})

	err = templGoroutine.Execute(w, struct {
		Name            string
		PC              uint64
		N               int
		ExecTimePercent string
		MaxTotal        int64
		TotalExecTime   int64
		GList           []*trace.GDesc
	}{
		Name:            name,
		PC:              pc,
		N:               len(glist),
		ExecTimePercent: execTimePercent,
		MaxTotal:        maxTotalTime,
		TotalExecTime:   execTime,
		GList:           glist})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to execute template: %v", err), http.StatusInternalServerError)
		return
	}
}

var templGoroutine = template.Must(template.New("").Funcs(template.FuncMap{
	"prettyDuration": func(s trace.GExecutionStatEntry) template.HTML {
		d := time.Duration(s.Total) * time.Nanosecond
		return template.HTML(niceDuration(d))
	},
	"percent": func(dividened, divisor int64) template.HTML {
		if divisor == 0 {
			return ""
		}
		return template.HTML(fmt.Sprintf("(%.1f%%)", float64(dividened)/float64(divisor)*100))
	},
	"minavgmax": func(s trace.GExecutionStatEntry) template.HTML {
		if s.Count == 0 {
			return ""
		}
		d := time.Duration(s.Total/s.Count) * time.Nanosecond
		min := time.Duration(s.Min) * time.Nanosecond
		max := time.Duration(s.Max) * time.Nanosecond
		return "<small>" + template.HTML("["+niceDuration(min)+"/"+niceDuration(d)+"/"+niceDuration(max)+"]") + "</small>"
	},
	"barLen": func(s trace.GExecutionStatEntry, total trace.GExecutionStatEntry) template.HTML {
		if total.Total == 0 {
			return "0"
		}
		return template.HTML(fmt.Sprintf("%.2f%%", float64(s.Total)/float64(total.Total)*100))
	},
	"unknownTime": func(desc *trace.GExecutionStat) trace.GExecutionStatEntry {
		sum := desc.ExecTime.Total + desc.IOTime.Total + desc.BlockTime.Total + desc.SyscallTime.Total + desc.SchedWaitTime.Total
		if sum < desc.TotalTime.Total {
			e := trace.GExecutionStatEntry{}
			e.Total = desc.TotalTime.Total - sum
			return e
		}
		return trace.GExecutionStatEntry{}
	},
}).Parse(`
<!DOCTYPE html>
<title>Goroutine {{.Name}}</title>
<style>
th {
  background-color: #050505;
  color: #fff;
}
table {
  border-collapse: collapse;
}
.details tr:hover {
  background-color: #f2f2f2;
}
.details td {
  text-align: right;
  border: 1px solid black;
}
.details td.id {
  text-align: left;
}
.stacked-bar-graph {
  width: 300px;
  height: 10px;
  color: #414042;
  white-space: nowrap;
  font-size: 5px;
}
.stacked-bar-graph span {
  display: inline-block;
  width: 100%;
  height: 100%;
  box-sizing: border-box;
  float: left;
  padding: 0;
}
.unknown-time { background-color: #636363; }
.exec-time { background-color: #d7191c; }
.io-time { background-color: #fdae61; }
.block-time { background-color: #d01c8b; }
.syscall-time { background-color: #7b3294; }
.sched-time { background-color: #2c7bb6; }
</style>

<script>
function reloadTable(key, value) {
  let params = new URLSearchParams(window.location.search);
  params.set(key, value);
  window.location.search = params.toString();
}
</script>

<table class="summary">
	<tr><td>Goroutine Name:</td><td>{{.Name}}</td></tr>
	<tr><td>Number of Goroutines:</td><td>{{.N}}</td></tr>
	<tr><td>Execution Time:</td><td>{{.ExecTimePercent}} of total program execution time </td> </tr>
	<tr><td>Network Wait Time:</td><td> <a href="/io?id={{.PC}}">graph</a><a href="/io?id={{.PC}}&raw=1" download="io.profile">(download)</a></td></tr>
	<tr><td>Sync Block Time:</td><td> <a href="/block?id={{.PC}}">graph</a><a href="/block?id={{.PC}}&raw=1" download="block.profile">(download)</a></td></tr>
	<tr><td>Blocking Syscall Time:</td><td> <a href="/syscall?id={{.PC}}">graph</a><a href="/syscall?id={{.PC}}&raw=1" download="syscall.profile">(download)</a></td></tr>
	<tr><td>Scheduler Wait Time:</td><td> <a href="/sched?id={{.PC}}">graph</a><a href="/sched?id={{.PC}}&raw=1" download="sched.profile">(download)</a></td></tr>
</table>
<p>
<table class="details">
<tr>
<th> Goroutine</th>
<th onclick="reloadTable('sortby', 'TotalTime')"> Total</th>
<th></th>
<th onclick="reloadTable('sortby', 'ExecTime')" class="exec-time"> Execution</th>
<th onclick="reloadTable('sortby', 'IOTime')" class="io-time"> Network wait</th>
<th onclick="reloadTable('sortby', 'BlockTime')" class="block-time"> Sync block </th>
<th onclick="reloadTable('sortby', 'SyscallTime')" class="syscall-time"> Blocking syscall</th>
<th onclick="reloadTable('sortby', 'SchedWaitTime')" class="sched-time"> Scheduler wait</th>
<th onclick="reloadTable('sortby', 'SweepTime')"> GC sweeping</th>
<th onclick="reloadTable('sortby', 'GCTime')"> GC pause</th>
</tr>
{{range .GList}}
  <tr>
    <td> <a href="/trace?goid={{.ID}}">{{.ID}}</a> </td>
    <td> {{prettyDuration .TotalTime}} </td>
    <td>
	<div class="stacked-bar-graph">
	  {{if unknownTime .GExecutionStat}}<span style="width:{{barLen (unknownTime .GExecutionStat) .TotalTime}}" class="unknown-time">&nbsp;</span>{{end}}
          {{if .ExecTime.Count}}<span style="width:{{barLen .ExecTime .TotalTime}}" class="exec-time">&nbsp;</span>{{end}}
          {{if .IOTime.Count}}<span style="width:{{barLen .IOTime .TotalTime}}" class="io-time">&nbsp;</span>{{end}}
          {{if .BlockTime.Count}}<span style="width:{{barLen .BlockTime .TotalTime}}" class="block-time">&nbsp;</span>{{end}}
          {{if .SyscallTime.Count}}<span style="width:{{barLen .SyscallTime .TotalTime}}" class="syscall-time">&nbsp;</span>{{end}}
          {{if .SchedWaitTime.Count}}<span style="width:{{barLen .SchedWaitTime .TotalTime}}" class="sched-time">&nbsp;</span>{{end}}
        </div>
    </td>
    <td> {{prettyDuration .ExecTime}}  {{percent .ExecTime.Total $.TotalExecTime}} {{minavgmax .ExecTime}}</td>
    <td> {{prettyDuration .IOTime}} {{minavgmax .IOTime}}</td>
    <td> {{prettyDuration .BlockTime}} {{minavgmax .BlockTime}}</td>
    <td> {{prettyDuration .SyscallTime}} {{minavgmax .SyscallTime}}</td>
    <td> {{prettyDuration .SchedWaitTime}} {{minavgmax .SchedWaitTime}}</td>
    <td> {{prettyDuration .SweepTime}} {{percent .SweepTime.Total .TotalTime.Total}}</td>
    <td> {{prettyDuration .GCTime}} {{percent .GCTime.Total .TotalTime.Total}} {{minavgmax .GCTime}}</td>
  </tr>
{{end}}
</table>
`))
