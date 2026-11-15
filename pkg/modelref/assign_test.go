package modelref

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func group(owner string, refs ...string) Group[string] {
	g := Group[string]{Owner: owner}
	for _, r := range refs {
		g.Refs = append(g.Refs, MustParse(r))
	}
	return g
}

var (
	opusBedrock     = ConcreteBinding{"anthropic", "claude-opus-4-7", "bedrock"}
	opusAnthropic   = ConcreteBinding{"anthropic", "claude-opus-4-7", "anthropic"}
	opusVertex      = ConcreteBinding{"anthropic", "claude-opus-4-7", "vertex"}
	sonnetBedrock   = ConcreteBinding{"anthropic", "claude-sonnet-4-6", "bedrock"}
	sonnetAnthropic = ConcreteBinding{"anthropic", "claude-sonnet-4-6", "anthropic"}
	gptOpenAI       = ConcreteBinding{"openai", "gpt-5", "openai"}
	assignCatalog   = []ConcreteBinding{opusBedrock, opusAnthropic, opusVertex, sonnetBedrock, sonnetAnthropic, gptOpenAI}
)

func TestAssign(t *testing.T) {
	type contested struct {
		Binding ConcreteBinding
		Winner  string
		Losers  []string
	}
	for _, tc := range []struct {
		name   string
		groups []Group[string]
		// Under specificity the most specific covering ref wins; under first-wins the first group to cover does.
		bySpecificity, byOrder                   map[string][]ConcreteBinding
		contestedBySpecificity, contestedByOrder []contested
	}{
		{
			name:   "a host-anchored ref outranks a model-anchored one",
			groups: []Group[string]{group("B", "anthropic/claude-opus-4-7"), group("A", "anthropic@bedrock"), group("idle", "@nowhere")},
			bySpecificity: map[string][]ConcreteBinding{
				"A": {opusBedrock, sonnetBedrock}, "B": {opusAnthropic, opusVertex}, "idle": {},
			},
			contestedBySpecificity: []contested{{opusBedrock, "A", []string{"B"}}},
			byOrder: map[string][]ConcreteBinding{
				"B": {opusBedrock, opusAnthropic, opusVertex}, "A": {sonnetBedrock}, "idle": {},
			},
			contestedByOrder: []contested{{opusBedrock, "B", []string{"A"}}},
		},
		{
			name:   "equal scores go to the group declared first",
			groups: []Group[string]{group("A", "anthropic/claude-opus-4-7@bedrock"), group("B", "anthropic/claude-opus-4-7@bedrock")},
			bySpecificity: map[string][]ConcreteBinding{
				"A": {opusBedrock}, "B": {},
			},
			contestedBySpecificity: []contested{{opusBedrock, "A", []string{"B"}}},
			byOrder:                map[string][]ConcreteBinding{"A": {opusBedrock}, "B": {}},
			contestedByOrder:       []contested{{opusBedrock, "A", []string{"B"}}},
		},
		{
			name:   "a group scores by its most specific covering ref",
			groups: []Group[string]{group("B", "anthropic@bedrock"), group("A", "anthropic", "anthropic/claude-opus-4-7@bedrock"), group("C", "@bedrock")},
			bySpecificity: map[string][]ConcreteBinding{
				"A": {opusBedrock, opusAnthropic, opusVertex, sonnetAnthropic}, "B": {sonnetBedrock}, "C": {},
			},
			contestedBySpecificity: []contested{{opusBedrock, "A", []string{"B", "C"}}, {sonnetBedrock, "B", []string{"A", "C"}}},
			byOrder: map[string][]ConcreteBinding{
				"B": {opusBedrock, sonnetBedrock}, "A": {opusAnthropic, opusVertex, sonnetAnthropic}, "C": {},
			},
			contestedByOrder: []contested{{opusBedrock, "B", []string{"A", "C"}}, {sonnetBedrock, "B", []string{"A", "C"}}},
		},
		{
			name:                   "uncovered bindings stay unassigned",
			groups:                 []Group[string]{group("A", "openai")},
			bySpecificity:          map[string][]ConcreteBinding{"A": {gptOpenAI}},
			contestedBySpecificity: []contested{},
			byOrder:                map[string][]ConcreteBinding{"A": {gptOpenAI}},
			contestedByOrder:       []contested{},
		},
		{
			name:                   "no groups",
			bySpecificity:          map[string][]ConcreteBinding{},
			contestedBySpecificity: []contested{},
			byOrder:                map[string][]ConcreteBinding{},
			contestedByOrder:       []contested{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assigned, carveouts := AssignSpecificityWins(tc.groups, assignCatalog)
			gotCarve := make([]contested, 0, len(carveouts))
			for _, c := range carveouts {
				gotCarve = append(gotCarve, contested(c))
			}
			if diff := cmp.Diff(tc.bySpecificity, assigned); diff != "" {
				t.Errorf("AssignSpecificityWins assignments (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.contestedBySpecificity, gotCarve); diff != "" {
				t.Errorf("AssignSpecificityWins carveouts (-want +got):\n%s", diff)
			}

			assigned, conflicts := AssignFirstWins(tc.groups, assignCatalog)
			gotConflicts := make([]contested, 0, len(conflicts))
			for _, c := range conflicts {
				gotConflicts = append(gotConflicts, contested(c))
			}
			if diff := cmp.Diff(tc.byOrder, assigned); diff != "" {
				t.Errorf("AssignFirstWins assignments (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.contestedByOrder, gotConflicts); diff != "" {
				t.Errorf("AssignFirstWins conflicts (-want +got):\n%s", diff)
			}
		})
	}
}
