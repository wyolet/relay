package clientprofile

import "fmt"

// Registry holds the profiles a deployment serves. Built once at boot from
// the composition root; read-only afterwards, so Resolve needs no lock.
type Registry struct {
	byName map[string]Profile
	order  []Profile
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{byName: make(map[string]Profile)}
}

// Register adds p. The name doubles as a URL path segment and a header
// value, so it must be a lowercase DNS label and unique.
func (reg *Registry) Register(p Profile) error {
	if p == nil {
		return fmt.Errorf("clientprofile: nil profile")
	}
	name := p.Name()
	if err := validName(name); err != nil {
		return err
	}
	if _, dup := reg.byName[name]; dup {
		return fmt.Errorf("clientprofile: duplicate profile %q", name)
	}
	reg.byName[name] = p
	reg.order = append(reg.order, p)
	return nil
}

// Profiles returns the registered profiles in registration order.
func (reg *Registry) Profiles() []Profile {
	if reg == nil {
		return nil
	}
	return reg.order
}

// ByName looks a profile up by its registered name.
func (reg *Registry) ByName(name string) (Profile, bool) {
	if reg == nil {
		return nil, false
	}
	p, ok := reg.byName[name]
	return p, ok
}

// validName accepts a lowercase DNS label: [a-z0-9] separated by single
// hyphens, no leading or trailing hyphen, at most 63 characters.
func validName(name string) error {
	if name == "" {
		return fmt.Errorf("clientprofile: empty profile name")
	}
	if len(name) > 63 {
		return fmt.Errorf("clientprofile: profile name %q too long", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(name)-1 || name[i-1] == '-' {
				return fmt.Errorf("clientprofile: invalid profile name %q", name)
			}
		default:
			return fmt.Errorf("clientprofile: invalid profile name %q", name)
		}
	}
	return nil
}
