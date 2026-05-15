package modelref

// Group binds an owner identity to a list of refs the owner declares.
// Used by Assign* helpers to attribute concrete bindings to owners.
type Group[O comparable] struct {
	Owner O
	Refs  []Ref
}

// Carveout records a binding that more than one owner could have claimed.
// Winner is the assigned owner; Losers are the other owners that also
// covered it (in declared order).
type Carveout[O comparable] struct {
	Binding ConcreteBinding
	Winner  O
	Losers  []O
}

// Conflict mirrors Carveout for AssignFirstWins. Same fields, different
// resolution rule.
type Conflict[O comparable] struct {
	Binding ConcreteBinding
	Winner  O
	Losers  []O
}

// AssignSpecificityWins attributes each binding in catalog to one owner
// using ref specificity. Per binding:
//  1. Each group's score for this binding = max Specificity over its
//     covering refs (groups with no covering ref are skipped).
//  2. Winner = group with highest score; ties broken by groups order.
//  3. If more than one group qualified, the binding is also recorded
//     as a Carveout listing the losers.
//
// assignments maps every input owner (even those with zero bindings)
// to its owned bindings, in catalog order.
func AssignSpecificityWins[O comparable](groups []Group[O], catalog []ConcreteBinding) (map[O][]ConcreteBinding, []Carveout[O]) {
	assignments := make(map[O][]ConcreteBinding, len(groups))
	for _, g := range groups {
		assignments[g.Owner] = []ConcreteBinding{}
	}
	carveouts := make([]Carveout[O], 0)

	for _, c := range catalog {
		var (
			winnerIdx  = -1
			winnerScr  = -1
			eligibleIx []int
		)
		for i, g := range groups {
			best := -1
			for _, r := range g.Refs {
				if !r.Covers(c) {
					continue
				}
				s := Specificity(r)
				if s > best {
					best = s
				}
			}
			if best < 0 {
				continue
			}
			eligibleIx = append(eligibleIx, i)
			if best > winnerScr {
				winnerScr = best
				winnerIdx = i
			}
		}
		if winnerIdx < 0 {
			continue
		}
		winner := groups[winnerIdx].Owner
		assignments[winner] = append(assignments[winner], c)
		if len(eligibleIx) > 1 {
			losers := make([]O, 0, len(eligibleIx)-1)
			for _, i := range eligibleIx {
				if i == winnerIdx {
					continue
				}
				losers = append(losers, groups[i].Owner)
			}
			carveouts = append(carveouts, Carveout[O]{Binding: c, Winner: winner, Losers: losers})
		}
	}
	return assignments, carveouts
}

// AssignFirstWins is the simpler attribution: each binding goes to the
// first group (in declared order) that covers it. Conflicts list the
// bindings that more than one group could have claimed.
func AssignFirstWins[O comparable](groups []Group[O], catalog []ConcreteBinding) (map[O][]ConcreteBinding, []Conflict[O]) {
	assignments := make(map[O][]ConcreteBinding, len(groups))
	for _, g := range groups {
		assignments[g.Owner] = []ConcreteBinding{}
	}
	conflicts := make([]Conflict[O], 0)

	for _, c := range catalog {
		var (
			winnerIdx = -1
			eligible  []int
		)
		for i, g := range groups {
			covered := false
			for _, r := range g.Refs {
				if r.Covers(c) {
					covered = true
					break
				}
			}
			if !covered {
				continue
			}
			eligible = append(eligible, i)
			if winnerIdx < 0 {
				winnerIdx = i
			}
		}
		if winnerIdx < 0 {
			continue
		}
		winner := groups[winnerIdx].Owner
		assignments[winner] = append(assignments[winner], c)
		if len(eligible) > 1 {
			losers := make([]O, 0, len(eligible)-1)
			for _, i := range eligible {
				if i == winnerIdx {
					continue
				}
				losers = append(losers, groups[i].Owner)
			}
			conflicts = append(conflicts, Conflict[O]{Binding: c, Winner: winner, Losers: losers})
		}
	}
	return assignments, conflicts
}
