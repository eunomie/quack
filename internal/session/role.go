package session

// Role is the trust level of the user who issued a request. The zero value is
// RoleOwner so any code path that doesn't set a role keeps today's full-access
// behavior (owner sessions, infer/naming one-shots, tests).
type Role int

const (
	RoleOwner Role = iota
	RoleGuest
)

func (r Role) IsGuest() bool { return r == RoleGuest }
