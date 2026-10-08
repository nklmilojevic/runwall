package wfgraph

import (
	"testing"
)

const deployBranch = `
on: pull_request
jobs:
  build-arch:
    strategy:
      matrix:
        arch: [amd64, arm64]
    name: Build ${{ matrix.arch }}
    runs-on: ubuntu-latest
    steps: [{run: make}]
  merge:
    needs: build-arch
    runs-on: ubuntu-latest
    steps: [{run: make}]
  lint:
    runs-on: ubuntu-latest
    steps: [{run: make}]
  deploy_branch:
    needs: [merge, lint]
    uses: ./.github/workflows/deploy.yaml
`

func TestBuildMatchesJobsAndLaysOutLevels(t *testing.T) {
	defs, err := Parse(deployBranch)
	if err != nil {
		t.Fatal(err)
	}
	g := Build(defs, []Job{
		{ID: 1, Name: "Build amd64", State: "ok"},
		{ID: 2, Name: "Build arm64", State: "run"},
		{ID: 3, Name: "lint", State: "ok"},
		{ID: 4, Name: "deploy_branch / helm", State: "queue"},
		{ID: 5, Name: "something else", State: "ok"},
	})
	byID := map[string]*Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	levels := map[string]int{"build-arch": 0, "lint": 0, "merge": 1, "deploy_branch": 2}
	for id, want := range levels {
		if byID[id].Level != want {
			t.Errorf("%s: level %d, want %d", id, byID[id].Level, want)
		}
	}
	m := byID["build-arch"]
	if !m.Matrix || len(m.Jobs) != 2 || m.Done() != 1 || m.State(true) != "run" {
		t.Errorf("matrix node: %+v state %s", m.Jobs, m.State(true))
	}
	if s := byID["merge"].State(true); s != "pending" {
		t.Errorf("merge has not started, got %s", s)
	}
	if s := byID["merge"].State(false); s != "skip" {
		t.Errorf("merge never ran in a finished run, got %s", s)
	}
	if len(byID["deploy_branch"].Jobs) != 1 {
		t.Error("reusable workflow jobs belong to their caller")
	}
	if len(g.Edges) != 3 {
		t.Errorf("edges: %d", len(g.Edges))
	}
	if byID["lint"].Y <= byID["build-arch"].Y || g.W != 3*NodeW+2*ColGap {
		t.Errorf("layout: lint y=%d, width %d", byID["lint"].Y, g.W)
	}
}

func TestLiteralNamesWinOverTemplates(t *testing.T) {
	defs, err := Parse("jobs:\n  a:\n    name: ${{ inputs.x }}\n  b:\n    name: build\n")
	if err != nil {
		t.Fatal(err)
	}
	g := Build(defs, []Job{{Name: "build", State: "ok"}})
	if len(g.Nodes[1].Jobs) != 1 || len(g.Nodes[0].Jobs) != 0 {
		t.Fatal("literal name should claim its job")
	}
}

func TestParseRejectsFilesWithoutJobs(t *testing.T) {
	if _, err := Parse("on: push\n"); err == nil {
		t.Fatal("want an error")
	}
}
