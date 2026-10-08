// Package wfgraph turns a workflow file and a run's jobs into a dependency graph, like the
// one GitHub shows on a run page, so jobs that haven't started yet are visible too.
package wfgraph

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Def is one job as declared in the workflow file.
type Def struct {
	ID     string
	Name   string // the job's name: field, or its ID
	Needs  []string
	Matrix bool
	match  *regexp.Regexp
	exact  bool // Name has no ${{ }} expressions
}

// Parse reads the jobs of a workflow file in declaration order.
func Parse(src string) ([]Def, error) {
	var doc struct {
		Jobs yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, err
	}
	if doc.Jobs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("workflow has no jobs")
	}
	var defs []Def
	for i := 0; i+1 < len(doc.Jobs.Content); i += 2 {
		id := doc.Jobs.Content[i].Value
		var job struct {
			Name     string    `yaml:"name"`
			Needs    yaml.Node `yaml:"needs"`
			Strategy struct {
				Matrix yaml.Node `yaml:"matrix"`
			} `yaml:"strategy"`
		}
		if err := doc.Jobs.Content[i+1].Decode(&job); err != nil {
			return nil, fmt.Errorf("job %s: %w", id, err)
		}
		d := Def{ID: id, Name: job.Name, Matrix: job.Strategy.Matrix.Kind != 0}
		if d.Name == "" {
			d.Name = id
		}
		switch job.Needs.Kind {
		case yaml.ScalarNode:
			d.Needs = []string{job.Needs.Value}
		case yaml.SequenceNode:
			for _, n := range job.Needs.Content {
				d.Needs = append(d.Needs, n.Value)
			}
		}
		d.match, d.exact = namePattern(d.Name)
		defs = append(defs, d)
	}
	return defs, nil
}

var expr = regexp.MustCompile(`\$\{\{.*?\}\}`)

// namePattern matches the names GitHub gives a declared job's runs: the name itself, with
// expressions filled in, a matrix suffix like " (ubuntu, 22)", or " / inner job" for a
// reusable workflow.
func namePattern(name string) (*regexp.Regexp, bool) {
	var b strings.Builder
	last := 0
	for _, loc := range expr.FindAllStringIndex(name, -1) {
		b.WriteString(regexp.QuoteMeta(name[last:loc[0]]))
		b.WriteString(".*?")
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(name[last:]))
	return regexp.MustCompile(`^` + b.String() + `( \(.*\))?( / .*)?$`), last == 0
}

// Job is the slice of a GitHub job the graph needs.
type Job struct {
	ID    int64
	Name  string
	State string
}

// Node is a declared job with the jobs GitHub created for it.
type Node struct {
	Def
	Jobs  []Job
	Level int
	X, Y  int
	H     int
}

// Edge joins a needed job to the job that waits for it.
type Edge struct {
	From, To *Node
}

// Path draws the edge as a curve from the right of From to the left of To.
func (e Edge) Path() string {
	x1, y1 := e.From.X+NodeW, e.From.Y+e.From.H/2
	x2, y2 := e.To.X, e.To.Y+e.To.H/2
	mid := (x1 + x2) / 2
	return fmt.Sprintf("M%d %d C%d %d %d %d %d %d", x1, y1, mid, y1, mid, y2, x2, y2)
}

type Graph struct {
	Nodes []*Node
	Edges []Edge
	W, H  int
}

// Layout sizes, in pixels.
const (
	NodeW   = 220
	NodeH   = 44
	MatrixH = 76
	ColGap  = 56
	RowGap  = 16
)

// Build matches jobs to their declarations and lays the graph out in columns, one per
// dependency level. Jobs that match no declaration are left out.
func Build(defs []Def, jobs []Job) Graph {
	nodes := make([]*Node, len(defs))
	byID := make(map[string]*Node, len(defs))
	for i, d := range defs {
		nodes[i] = &Node{Def: d}
		byID[d.ID] = nodes[i]
	}
	for _, j := range jobs {
		if n := matchJob(nodes, j.Name); n != nil {
			n.Jobs = append(n.Jobs, j)
		}
	}

	var level func(n *Node, seen map[string]bool) int
	level = func(n *Node, seen map[string]bool) int {
		if seen[n.ID] { // a cycle; GitHub rejects these, so just stop
			return 0
		}
		seen[n.ID] = true
		defer delete(seen, n.ID)
		l := 0
		for _, need := range n.Needs {
			if dep, ok := byID[need]; ok {
				l = max(l, level(dep, seen)+1)
			}
		}
		return l
	}

	g := Graph{Nodes: nodes}
	colY := map[int]int{}
	for _, n := range nodes {
		n.Level = level(n, map[string]bool{})
		n.H = NodeH
		if n.Matrix {
			n.H = MatrixH
		}
		n.X = n.Level * (NodeW + ColGap)
		n.Y = colY[n.Level]
		colY[n.Level] += n.H + RowGap
		g.W = max(g.W, n.X+NodeW)
		g.H = max(g.H, n.Y+n.H)
		for _, need := range n.Needs {
			if dep, ok := byID[need]; ok {
				g.Edges = append(g.Edges, Edge{From: dep, To: n})
			}
		}
	}
	return g
}

// matchJob prefers declarations whose name is literal, so "build" isn't claimed by a
// templated "${{ matrix.os }}".
func matchJob(nodes []*Node, name string) *Node {
	for _, exact := range []bool{true, false} {
		for _, n := range nodes {
			if n.exact == exact && n.match.MatchString(name) {
				return n
			}
		}
	}
	return nil
}

// stateRank orders job states from most to least important to show for a group.
var stateRank = []string{"fail", "stuck", "run", "queue", "wait", "cancel", "ok", "skip"}

// State sums up a node's jobs. A node with no jobs yet is "pending" while the run is
// active, and "skip" once it finished without them.
func (n *Node) State(runActive bool) string {
	if len(n.Jobs) == 0 {
		if runActive {
			return "pending"
		}
		return "skip"
	}
	best := len(stateRank)
	for _, j := range n.Jobs {
		for i, s := range stateRank {
			if s == j.State && i < best {
				best = i
			}
		}
	}
	if best == len(stateRank) {
		return n.Jobs[0].State
	}
	return stateRank[best]
}

// Done counts the node's jobs that finished.
func (n *Node) Done() int {
	d := 0
	for _, j := range n.Jobs {
		switch j.State {
		case "ok", "fail", "cancel", "skip":
			d++
		}
	}
	return d
}
