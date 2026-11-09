package plan

// inputs records what a plan was built from: pointers to immutable rows and plans, and the few scalars it copies. Two plans built from equal inputs are interchangeable.
type inputs []any

func (a inputs) equal(b inputs) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SameOr returns prev when it was built from the same inputs as next, otherwise next. The engine compares outputs by pointer, so returning prev stops a rebuild from dirtying anything downstream.
func SameOr[T any, P interface {
	*T
	builtFrom() inputs
}](prev, next P) P {
	if prev != nil && prev.builtFrom().equal(next.builtFrom()) {
		return prev
	}
	return next
}
