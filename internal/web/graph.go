package web

import (
	"context"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/wfgraph"
)

// graphFailTTL is how long a workflow file that couldn't be read or parsed is left alone.
// Files read at a commit never change, so successes are kept until the cache fills.
const (
	graphFailTTL  = 10 * time.Minute
	graphCacheMax = 500
)

type graphEntry struct {
	defs []wfgraph.Def
	ok   bool
	at   time.Time
}

// workflowDefs returns the parsed jobs of a run's workflow file at its commit, or nil when
// the file can't be read (no Contents permission, a dynamic workflow) or parsed.
func (s *Server) workflowDefs(ctx context.Context, run store.Run) []wfgraph.Def {
	key := strconv.FormatInt(run.RepoID, 10) + ":" + run.HeadSHA + ":" + run.WorkflowPath
	now := s.Now()
	s.graphMu.Lock()
	e, hit := s.graphs[key]
	s.graphMu.Unlock()
	if hit && (e.ok || now.Sub(e.at) < graphFailTTL) {
		return e.defs
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e = graphEntry{at: now}
	if src, err := s.Syncer.WorkflowFile(ctx, run.ID); err == nil {
		if e.defs, err = wfgraph.Parse(src); err == nil {
			e.ok = true
		}
	}
	s.graphMu.Lock()
	if s.graphs == nil || len(s.graphs) >= graphCacheMax {
		s.graphs = make(map[string]graphEntry)
	}
	s.graphs[key] = e
	s.graphMu.Unlock()
	return e.defs
}

// runGraph is the dependency graph for the run page; nil when there is nothing to draw.
func (s *Server) runGraph(ctx context.Context, rv runView) *wfgraph.Graph {
	defs := s.workflowDefs(ctx, rv.Run.Run)
	if len(defs) < 2 {
		return nil
	}
	jobs := make([]wfgraph.Job, len(rv.Jobs))
	for i, j := range rv.Jobs {
		jobs[i] = wfgraph.Job{ID: j.ID, Name: j.Name, State: string(jobState(j, rv.Now, rv.Threshold))}
	}
	g := wfgraph.Build(defs, jobs)
	return &g
}

// workflowFileName is the file's base name, or the workflow's name when the path is unknown.
func workflowFileName(r store.FeedRun) string {
	p, _, _ := strings.Cut(r.WorkflowPath, "@")
	if p == "" {
		return r.WorkflowName
	}
	return path.Base(p)
}

// nodeLabel names a graph node: matrix groups by job ID, as GitHub does, and templated
// names by the job GitHub created when there is exactly one.
func nodeLabel(n *wfgraph.Node) string {
	switch {
	case n.Matrix:
		return "Matrix: " + n.ID
	case !strings.Contains(n.Name, "${{"):
		return n.Name
	case len(n.Jobs) == 1:
		return n.Jobs[0].Name
	}
	return n.ID
}

func nodeStyle(n *wfgraph.Node) string {
	return "left:" + strconv.Itoa(n.X) + "px;top:" + strconv.Itoa(n.Y) + "px;height:" + strconv.Itoa(n.H) + "px"
}

func graphSize(g *wfgraph.Graph) string {
	return "width:" + strconv.Itoa(g.W) + "px;height:" + strconv.Itoa(g.H) + "px"
}
