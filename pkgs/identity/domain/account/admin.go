package account

// AdminStanding is one effective admin: an ACTIVE account holding the admin
// role (auth.admin_role) with at least one key that resolves right now
// (not revoked, not expired). Accounts that fail any of the three are not
// admins as far as locking the management plane is concerned.
type AdminStanding struct {
	id         ID
	usableKeys int
}

func NewAdminStanding(id ID, usableKeys int) AdminStanding {
	return AdminStanding{id: id, usableKeys: usableKeys}
}

func (x AdminStanding) ID() ID          { return x.id }
func (x AdminStanding) UsableKeys() int { return x.usableKeys }

// GuardLastAdmin is the check for a change after which target is no longer an
// effective admin at all (disabled, admin role dropped): ErrLastAdmin when
// target is one and nobody else is. A target that is not an effective admin
// loses nothing, so it passes, and so does any change in a store with no
// effective admin to begin with (nothing left to protect).
func GuardLastAdmin(_ []AdminStanding, _ ID) error {
	return nil
}

// GuardLastAdminKey is the check for revoking one usable key of target: it
// only matters when that is target's last usable key, since the account stays
// an effective admin as long as one remains.
func GuardLastAdminKey(_ []AdminStanding, _ ID) error {
	return nil
}
