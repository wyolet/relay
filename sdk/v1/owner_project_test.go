package v1

import "testing"

func TestProjectOwner(t *testing.T) {
	o := ProjectOwner("p1")
	if !o.Tenant() || o.ID != "p1" {
		t.Fatalf("ProjectOwner(p1) = %+v", o)
	}
}
