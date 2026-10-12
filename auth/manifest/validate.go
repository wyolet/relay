package manifest

import (
	"errors"
	"fmt"
	"slices"

	"github.com/wyolet/relay/auth/rbac"
)

// check runs the rules a document must pass on its own, before any row is looked up.
func (r *Registry) check(d *Document) error {
	switch {
	case d.Role != nil:
		return r.checkRules(d)
	case d.User != nil:
		if field, err := d.User.check(); err != nil {
			return docError(d, field, err)
		}
	}
	return nil
}

// checkRules makes a rule term outside the vocabulary an error: a rule naming "models.updat" would otherwise match nothing and say nothing.
func (r *Registry) checkRules(d *Document) error {
	if len(d.Role.Rules) == 0 {
		return docError(d, "spec.rules", errors.New("at least one rule is required"))
	}
	for i, rule := range d.Role.Rules {
		path := item("spec.rules", i)
		if err := checkTerms(d, path+".kinds", "kind", rule.Kinds, r.cfg.RuleKinds); err != nil {
			return err
		}
		if err := checkTerms(d, path+".verbs", "verb", rule.Verbs, r.cfg.RuleVerbs); err != nil {
			return err
		}
	}
	return nil
}

func checkTerms(d *Document, path, what string, terms, vocabulary []string) error {
	if len(terms) == 0 {
		return docError(d, path, fmt.Errorf("at least one %s is required", what))
	}
	for j, t := range terms {
		switch {
		case t == "":
			return docError(d, item(path, j), fmt.Errorf("empty %s", what))
		case t != rbac.Wildcard && vocabulary != nil && !slices.Contains(vocabulary, t):
			return docError(d, item(path, j), fmt.Errorf("%q is not a registered %s", t, what))
		}
	}
	return nil
}
