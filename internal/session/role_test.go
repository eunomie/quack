package session

import "testing"

func TestRoleDefaultsToOwnerZeroValue(t *testing.T) {
	var r Role
	if r != RoleOwner {
		t.Fatalf("zero Role should be RoleOwner, got %v", r)
	}
	if !RoleGuest.IsGuest() {
		t.Fatal("RoleGuest.IsGuest() should be true")
	}
	if RoleOwner.IsGuest() {
		t.Fatal("RoleOwner.IsGuest() should be false")
	}
}
