package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Refusals the group tables can produce.
var (
	// ErrDirectoryGroupUnknown reports a group this organization does not have.
	ErrDirectoryGroupUnknown = errors.New("directory group unknown")
	// ErrDirectoryGroupExists reports a display name or external identifier already in use.
	ErrDirectoryGroupExists = errors.New("directory group already exists")
)

// DirectoryGroup is a group the customer's directory synchronised, and the role an
// administrator decided it grants here.
type DirectoryGroup struct {
	ID           uuid.UUID
	Organization string
	DisplayName  string
	ExternalID   string
	// Role is what belonging to this group earns. The zero role grants nothing, which is the
	// right default: a directory synchronises every group it is told to and most of them are
	// about something other than this product.
	Role      authz.Role
	Members   []uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DirectoryGroupList is a page of them.
type DirectoryGroupList struct {
	Groups []DirectoryGroup
	Total  int
}

// CreateDirectoryGroup records a group the directory reported.
func (p *Placements) CreateDirectoryGroup(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	displayName, externalID string, members []uuid.UUID,
) (DirectoryGroup, error) {
	return audited(ctx, p, principal, organization, audit.ActionMembershipGranted,
		func(ctx context.Context, transaction pgx.Tx) (
			DirectoryGroup, audit.Target, audit.Detail, error,
		) {
			var id uuid.UUID
			if err := transaction.QueryRow(ctx, `
				INSERT INTO scim_group (group_id, org_id, display_name, external_id)
				VALUES ($1, $2, $3, $4)
				RETURNING group_id`,
				uuid.New(), organization.String(), displayName,
				nullableText(externalID)).Scan(&id); err != nil {
				if isUniqueViolation(err, "scim_group_name_is_unique_per_org") ||
					isUniqueViolation(err, "scim_group_external_id_is_unique_per_org") {
					return DirectoryGroup{}, audit.Target{}, nil, ErrDirectoryGroupExists
				}
				return DirectoryGroup{}, audit.Target{}, nil,
					fmt.Errorf("creating a directory group: %w", err)
			}
			if err := setGroupMembers(ctx, transaction, organization, id, members); err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}

			group, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}
			return group,
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{
					"group":   group.DisplayName,
					"members": len(group.Members),
					"source":  SourceSCIM.String(),
					// The role is empty here and says so, because a group that grants nothing
					// until somebody maps it is the fact an administrator needs to notice.
					"grants": string(group.Role),
				}, nil
		})
}

// ReplaceDirectoryGroup applies the directory's whole picture of a group, members included.
func (p *Placements) ReplaceDirectoryGroup(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, displayName, externalID string, members []uuid.UUID,
) (DirectoryGroup, error) {
	return audited(ctx, p, principal, organization, audit.ActionMembershipChanged,
		func(ctx context.Context, transaction pgx.Tx) (
			DirectoryGroup, audit.Target, audit.Detail, error,
		) {
			before, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}
			if _, err := transaction.Exec(ctx, `
				UPDATE scim_group
				   SET display_name = $3, external_id = $4, updated_at = now()
				 WHERE org_id = $1 AND group_id = $2`,
				organization.String(), id, displayName, nullableText(externalID)); err != nil {
				if isUniqueViolation(err, "scim_group_name_is_unique_per_org") ||
					isUniqueViolation(err, "scim_group_external_id_is_unique_per_org") {
					return DirectoryGroup{}, audit.Target{}, nil, ErrDirectoryGroupExists
				}
				return DirectoryGroup{}, audit.Target{}, nil,
					fmt.Errorf("updating a directory group: %w", err)
			}
			if err := setGroupMembers(ctx, transaction, organization, id, members); err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}

			after, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{
					"group":  after.DisplayName,
					"source": SourceSCIM.String(),
					"before": map[string]any{"members": len(before.Members)},
					"after":  map[string]any{"members": len(after.Members)},
				}, nil
		})
}

// ChangeDirectoryGroupMembers adds and removes people, which is what a directory sends far more
// often than a whole replacement.
func (p *Placements) ChangeDirectoryGroupMembers(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, added, removed []uuid.UUID,
) (DirectoryGroup, error) {
	return audited(ctx, p, principal, organization, audit.ActionMembershipChanged,
		func(ctx context.Context, transaction pgx.Tx) (
			DirectoryGroup, audit.Target, audit.Detail, error,
		) {
			group, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}

			for _, member := range added {
				if _, err := transaction.Exec(ctx, `
					INSERT INTO scim_group_member (group_id, org_id, user_id)
					VALUES ($1, $2, $3)
					ON CONFLICT DO NOTHING`,
					id, organization.String(), member); err != nil {
					return DirectoryGroup{}, audit.Target{}, nil,
						fmt.Errorf("adding a group member: %w", err)
				}
			}
			for _, member := range removed {
				if _, err := transaction.Exec(ctx, `
					DELETE FROM scim_group_member
					 WHERE group_id = $1 AND org_id = $2 AND user_id = $3`,
					id, organization.String(), member); err != nil {
					return DirectoryGroup{}, audit.Target{}, nil,
						fmt.Errorf("removing a group member: %w", err)
				}
			}
			// Everybody the change touched, on both sides. Somebody removed from the group that
			// granted their role loses it here, in this transaction — which is the half of
			// story 14 that is not about deleting a person.
			if err := recomputeAll(ctx, transaction, organization,
				append(append([]uuid.UUID(nil), added...), removed...)); err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}

			after, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{
					"group":   group.DisplayName,
					"added":   len(added),
					"removed": len(removed),
					"source":  SourceSCIM.String(),
				}, nil
		})
}

// MapDirectoryGroupToRole is the one decision in this whole surface that is an
// ADMINISTRATOR'S rather than a directory's: what belonging to a group means here.
//
// It is deliberately not something the directory can set. A directory reports who is in the
// company; if it could also decide what they may do, a change in somebody else's system would
// be a privilege grant in this one.
func (p *Placements) MapDirectoryGroupToRole(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, role authz.Role,
) (DirectoryGroup, error) {
	return audited(ctx, p, principal, organization, audit.ActionMembershipChanged,
		func(ctx context.Context, transaction pgx.Tx) (
			DirectoryGroup, audit.Target, audit.Detail, error,
		) {
			before, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}
			if _, err := transaction.Exec(ctx, `
				UPDATE scim_group SET role = $3, updated_at = now()
				 WHERE org_id = $1 AND group_id = $2`,
				organization.String(), id, nullableText(string(role))); err != nil {
				return DirectoryGroup{}, audit.Target{}, nil,
					fmt.Errorf("mapping a directory group: %w", err)
			}
			// Everybody in it, now. Mapping a group and then waiting for each member to sign in
			// again would make the map a thing that takes effect at an unpredictable time.
			if err := recomputeAll(ctx, transaction, organization, before.Members); err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}

			after, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return DirectoryGroup{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{
					"group":   after.DisplayName,
					"members": len(after.Members),
					"before":  string(before.Role),
					"after":   string(after.Role),
				}, nil
		})
}

// DeleteDirectoryGroup removes a group and recomputes what its members are left holding.
func (p *Placements) DeleteDirectoryGroup(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionMembershipRevoked,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			before, err := readDirectoryGroup(ctx, transaction, organization, id)
			if err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if _, err := transaction.Exec(ctx, `
				DELETE FROM scim_group WHERE org_id = $1 AND group_id = $2`,
				organization.String(), id); err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("deleting a directory group: %w", err)
			}
			if err := recomputeAll(ctx, transaction, organization, before.Members); err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{
					"group":   before.DisplayName,
					"granted": string(before.Role),
					"members": len(before.Members),
				}, nil
		})
	return err
}

// DirectoryGroups lists what the directory has synchronised.
func (p *Placements) DirectoryGroups(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	displayName string, startIndex, count int,
) (DirectoryGroupList, error) {
	if !principal.MemberOf(organization) {
		return DirectoryGroupList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return DirectoryGroupList{}, err
	}

	var list DirectoryGroupList
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM scim_group
		 WHERE org_id = $1 AND ($2::TEXT IS NULL OR display_name = $2::TEXT)`,
		organization.String(), nullableText(displayName)).Scan(&list.Total); err != nil {
		return DirectoryGroupList{}, fmt.Errorf("counting directory groups: %w", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT group_id, display_name, external_id, role, created_at, updated_at
		  FROM scim_group
		 WHERE org_id = $1 AND ($2::TEXT IS NULL OR display_name = $2::TEXT)
		 ORDER BY created_at, group_id
		 OFFSET $3 LIMIT $4`,
		organization.String(), nullableText(displayName), max(startIndex-1, 0), pageLimit(count))
	if err != nil {
		return DirectoryGroupList{}, fmt.Errorf("listing directory groups: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		group, scanErr := scanDirectoryGroup(rows, organization.String())
		if scanErr != nil {
			return DirectoryGroupList{}, scanErr
		}
		list.Groups = append(list.Groups, group)
	}
	if err := rows.Err(); err != nil {
		return DirectoryGroupList{}, fmt.Errorf("listing directory groups: %w", err)
	}

	// The members are read per group rather than in one join, because a listing of groups with
	// their members flattened would return one row per membership and the paging would be over
	// the wrong thing.
	for index := range list.Groups {
		members, memberErr := groupMembers(ctx, pool, organization, list.Groups[index].ID)
		if memberErr != nil {
			return DirectoryGroupList{}, memberErr
		}
		list.Groups[index].Members = members
	}
	return list, nil
}

// DirectoryGroup reads one.
func (p *Placements) DirectoryGroup(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) (DirectoryGroup, error) {
	if !principal.MemberOf(organization) {
		return DirectoryGroup{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return DirectoryGroup{}, err
	}
	return readDirectoryGroup(ctx, pool, organization, id)
}

func readDirectoryGroup(
	ctx context.Context, on querier, organization tenancy.Organization, id uuid.UUID,
) (DirectoryGroup, error) {
	row := on.QueryRow(ctx, `
		SELECT group_id, display_name, external_id, role, created_at, updated_at
		  FROM scim_group
		 WHERE org_id = $1 AND group_id = $2`, organization.String(), id)

	group, err := scanDirectoryGroup(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return DirectoryGroup{}, ErrDirectoryGroupUnknown
	}
	if err != nil {
		return DirectoryGroup{}, err
	}
	group.Members, err = groupMembers(ctx, on, organization, id)
	return group, err
}

func scanDirectoryGroup(row scanned, organization string) (DirectoryGroup, error) {
	var (
		group    DirectoryGroup
		external *string
		role     *string
	)
	if err := row.Scan(&group.ID, &group.DisplayName, &external, &role,
		&group.CreatedAt, &group.UpdatedAt); err != nil {
		return DirectoryGroup{}, err
	}
	group.Organization = organization
	group.ExternalID = orEmptyText(external)
	group.Role = authz.Role(orEmptyText(role))
	return group, nil
}

func groupMembers(
	ctx context.Context, on querier, organization tenancy.Organization, id uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := on.Query(ctx, `
		SELECT user_id FROM scim_group_member
		 WHERE org_id = $1 AND group_id = $2
		 ORDER BY user_id`, organization.String(), id)
	if err != nil {
		return nil, fmt.Errorf("reading group members: %w", err)
	}
	defer rows.Close()

	members := make([]uuid.UUID, 0, 8)
	for rows.Next() {
		var member uuid.UUID
		if err := rows.Scan(&member); err != nil {
			return nil, fmt.Errorf("scanning a group member: %w", err)
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

// setGroupMembers makes the group's membership exactly what was asked for, and recomputes every
// person on either side of the change.
func setGroupMembers(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	id uuid.UUID, members []uuid.UUID,
) error {
	before, err := groupMembers(ctx, transaction, organization, id)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `
		DELETE FROM scim_group_member WHERE org_id = $1 AND group_id = $2`,
		organization.String(), id); err != nil {
		return fmt.Errorf("clearing group members: %w", err)
	}
	for _, member := range members {
		if _, err := transaction.Exec(ctx, `
			INSERT INTO scim_group_member (group_id, org_id, user_id)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			id, organization.String(), member); err != nil {
			return fmt.Errorf("setting group members: %w", err)
		}
	}
	return recomputeAll(ctx, transaction, organization, append(before, members...))
}

func recomputeAll(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	people []uuid.UUID,
) error {
	seen := make(map[uuid.UUID]bool, len(people))
	for _, person := range people {
		if seen[person] {
			continue
		}
		seen[person] = true
		if err := recomputeProvisionedRole(ctx, transaction, organization, person); err != nil {
			return err
		}
	}
	return nil
}

// recomputeProvisionedRole sets a directory-owned membership's role to what the person's groups
// say it should be.
//
// STRONGEST rather than first, for the reason a token's group claim is read that way: somebody
// in both an administrators group and a viewers group is an administrator, and picking whichever
// the directory happened to list first would make their access depend on an ordering nobody
// controls.
//
// It touches ONLY memberships the directory owns. A role an administrator granted by hand
// survives a synchronisation — otherwise connecting a directory would silently revoke every
// deliberate grant in the tenant, which is the kind of change somebody discovers during an
// incident.
func recomputeProvisionedRole(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, user uuid.UUID,
) error {
	rows, err := transaction.Query(ctx, `
		SELECT scim_group.role
		  FROM scim_group_member
		  JOIN scim_group ON scim_group.group_id = scim_group_member.group_id
		 WHERE scim_group_member.org_id = $1
		   AND scim_group_member.user_id = $2
		   AND scim_group.role IS NOT NULL`, organization.String(), user)
	if err != nil {
		return fmt.Errorf("reading a person's groups: %w", err)
	}
	defer rows.Close()

	held := make(map[authz.Role]bool, 4)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning a group role: %w", err)
		}
		if role, known := authz.ParseRole(name); known {
			held[role] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading a person's groups: %w", err)
	}

	// authz.Roles() is ordered most privileged first.
	strongest := authz.Role("")
	for _, role := range authz.Roles() {
		if held[role] {
			strongest = role
			break
		}
	}

	// Nobody's groups grant anything: the membership holds NO ROLE, and the row stays. It does
	// NOT become inactive — active is the directory's statement about the person, and this
	// function has no business contradicting it. A directory reading its people back must find
	// them exactly as it left them.
	if strongest == "" {
		if _, err := transaction.Exec(ctx, `
			UPDATE organization_membership
			   SET role = NULL, updated_at = now()
			 WHERE org_id = $1 AND user_id = $2 AND source = $3`,
			organization.String(), user, int16(SourceSCIM)); err != nil {
			return fmt.Errorf("clearing a provisioned role: %w", err)
		}
		// The sessions that rested on the role go with it, in this transaction. Somebody
		// removed from the group that granted their access must lose it on their next request,
		// which is the half of story 14 that is not about deleting a person.
		if _, err := transaction.Exec(ctx, `
			UPDATE operator_session
			   SET revoked_at = now(), revoked_by = 'directory synchronisation'
			 WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL`,
			user, organization.String()); err != nil {
			return fmt.Errorf("revoking sessions: %w", err)
		}
		return nil
	}

	if _, err := transaction.Exec(ctx, `
		UPDATE organization_membership
		   SET role = $3, updated_at = now()
		 WHERE org_id = $1 AND user_id = $2 AND source = $4`,
		organization.String(), user, string(strongest), int16(SourceSCIM)); err != nil {
		return fmt.Errorf("setting a provisioned role: %w", err)
	}
	return nil
}
