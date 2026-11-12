package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

// KeyEnvironmentDuplicateError rejects a key list where two lines are the same
// key once trimmed: both would be one account with two environments.
type KeyEnvironmentDuplicateError struct {
	Index      int `json:"index"`
	FirstIndex int `json:"first_index"`
}

func (e *KeyEnvironmentDuplicateError) Error() string {
	return fmt.Sprintf("key #%d repeats key #%d", e.Index+1, e.FirstIndex+1)
}

// KeyEnvironmentConflictError rejects a key that already belongs to another
// independent key environment channel: one account cannot have two.
type KeyEnvironmentConflictError struct {
	Index       int    `json:"index"`
	ChannelId   int    `json:"channel_id"`
	ChannelName string `json:"channel_name"`
}

func (e *KeyEnvironmentConflictError) Error() string {
	return fmt.Sprintf("key #%d is already used by independent key environment channel %q (#%d)", e.Index+1, e.ChannelName, e.ChannelId)
}

// KeyEnvironmentShortageError reports keys left without a version because the
// pool has fewer captured versions than the channel has keys. Indexes are
// 0-based like the multi-key status list.
type KeyEnvironmentShortageError struct {
	Available         int      `json:"available"`
	Keys              int      `json:"keys"`
	UnassignedIndexes []int    `json:"unassigned_indexes"`
	Versions          []string `json:"versions"`
}

func (e *KeyEnvironmentShortageError) Error() string {
	return fmt.Sprintf("only %d captured versions for %d keys; %d keys would stay unassigned", e.Available, e.Keys, len(e.UnassignedIndexes))
}

// KeyEnvironmentAssignment is one key's planned environment. ProfileId is
// empty for an unassigned key.
type KeyEnvironmentAssignment struct {
	Index     int    `json:"index"`
	ProfileId string `json:"profile_id"`
	Previous  string `json:"previous_profile_id"`
	Downgrade bool   `json:"downgrade"`
	tag       string
}

// KeyEnvironmentFamily is the client family whose captured versions an
// independent key environment channel draws from, or "" for a client family
// without a pool.
func KeyEnvironmentFamily(clientProfile string) string {
	switch clientProfile {
	case "claude", "codex":
		return clientProfile
	}
	return ""
}

// KeyEnvironmentPool is the version pool a channel with these settings draws
// from, newest first.
func KeyEnvironmentPool(clientProfile, cacheTTL string) []*CLIVersionProfile {
	family := KeyEnvironmentFamily(clientProfile)
	if family == "" {
		return nil
	}
	return KeyEnvironmentProfiles(family, cacheTTL)
}

// planKeyEnvironments assigns versions to keys. Keys keep the version they
// have while it is still in the pool (a cache mode change maps it to the same
// version's other variant); keys without one take the newest free version in
// list order. reassign instead repacks every key: keys ordered by their current
// version, newest first, take pool[i].
func planKeyEnvironments(keys []string, existing map[string]string, pool []*CLIVersionProfile, reassign bool) ([]KeyEnvironmentAssignment, error) {
	plan := make([]KeyEnvironmentAssignment, 0, len(keys))
	seen := make(map[string]int, len(keys))
	for i, key := range keys {
		if strings.TrimSpace(key) == "" {
			continue
		}
		tag := model.ChannelKeyTag(key)
		if first, dup := seen[tag]; dup {
			return nil, &KeyEnvironmentDuplicateError{Index: i, FirstIndex: first}
		}
		seen[tag] = i
		plan = append(plan, KeyEnvironmentAssignment{Index: i, Previous: existing[tag], tag: tag})
	}

	inPool := make(map[string]*CLIVersionProfile, len(pool))
	byVersion := make(map[string]string, len(pool))
	for _, p := range pool {
		inPool[p.ID] = p
		byVersion[p.Version] = p.ID
	}
	// The version a key holds, carried over to the pool's variant of that
	// version, or "" once the version left the pool.
	current := func(previous string) string {
		if previous == "" {
			return ""
		}
		if _, ok := inPool[previous]; ok {
			return previous
		}
		if p := LookupCLIVersionProfile(previous); p != nil {
			return byVersion[p.Version]
		}
		return ""
	}

	taken := make(map[string]bool, len(pool))
	if reassign {
		order := make([]int, len(plan))
		for i := range order {
			order[i] = i
		}
		rank := func(previous string) string {
			if p := LookupCLIVersionProfile(current(previous)); p != nil {
				return p.Version
			}
			return ""
		}
		slices.SortStableFunc(order, func(a, b int) int {
			va, vb := rank(plan[a].Previous), rank(plan[b].Previous)
			switch {
			case va == "" && vb != "":
				return 1
			case va != "" && vb == "":
				return -1
			}
			return compareCLIVersions(vb, va)
		})
		for rankIndex, planIndex := range order {
			if rankIndex < len(pool) {
				plan[planIndex].ProfileId = pool[rankIndex].ID
				taken[pool[rankIndex].ID] = true
			}
		}
	} else {
		for i := range plan {
			id := current(plan[i].Previous)
			if id != "" && !taken[id] {
				plan[i].ProfileId = id
				taken[id] = true
			}
		}
		for i := range plan {
			if plan[i].ProfileId != "" {
				continue
			}
			for _, p := range pool {
				if !taken[p.ID] {
					plan[i].ProfileId = p.ID
					taken[p.ID] = true
					break
				}
			}
		}
	}

	for i := range plan {
		previous := LookupCLIVersionProfile(plan[i].Previous)
		next := LookupCLIVersionProfile(plan[i].ProfileId)
		plan[i].Downgrade = previous != nil && (next == nil || compareCLIVersions(next.Version, previous.Version) < 0)
	}
	return plan, nil
}

// ReconcileKeyEnvironments brings channel's key environment rows in line with
// its keys and settings. It is the only writer of the table and runs inside
// the transaction that saves the channel, so a rejected plan rolls the save
// back with it.
//
// With the switch off nothing is assigned, but rows of keys that left the
// channel are dropped so a deleted key does not stay held by it; the rows of
// remaining keys are kept for when the switch comes back on. With it on, a
// key list that repeats a key, or names a key another channel holds, is
// rejected; keys the pool cannot cover are rejected as a shortage unless
// allowUnassigned confirms them.
func ReconcileKeyEnvironments(tx *gorm.DB, channel *model.Channel, allowUnassigned bool, updates ...KeyEnvironmentUpdate) ([]KeyEnvironmentAssignment, error) {
	return reconcileKeyEnvironments(tx, channel, allowUnassigned, false, updates...)
}

// KeyEnvironmentUpdate changes one saved environment through the same
// transaction and validation as every other reconciliation. Nil fields keep
// their value; an empty profile leaves the key unassigned, an empty proxy
// inherits the channel proxy. TTL remains a channel-wide pool choice.
type KeyEnvironmentUpdate struct {
	Index     int
	ProfileId *string
	Proxy     *string
}

// ReassignKeyEnvironments repacks every key of the channel onto the pool,
// newest versions to the keys that hold the newest ones now.
func ReassignKeyEnvironments(tx *gorm.DB, channel *model.Channel, allowUnassigned bool) ([]KeyEnvironmentAssignment, error) {
	return reconcileKeyEnvironments(tx, channel, allowUnassigned, true)
}

// PreviewKeyEnvironmentReassignment is the plan ReassignKeyEnvironments would
// apply, without writing it.
func PreviewKeyEnvironmentReassignment(tx *gorm.DB, channel *model.Channel) ([]KeyEnvironmentAssignment, error) {
	setting := channel.GetSetting()
	existing, err := keyEnvironmentRows(tx, channel.Id)
	if err != nil {
		return nil, err
	}
	pool := KeyEnvironmentPool(setting.SyntheticClientHeadersProfile, setting.KeyEnvironmentCacheTTL)
	return planKeyEnvironments(channelKeyList(channel), existing, pool, true)
}

func reconcileKeyEnvironments(tx *gorm.DB, channel *model.Channel, allowUnassigned bool, reassign bool, updates ...KeyEnvironmentUpdate) ([]KeyEnvironmentAssignment, error) {
	setting := channel.GetSetting()
	existing, err := keyEnvironmentRows(tx, channel.Id)
	if err != nil {
		return nil, err
	}
	keys := channelKeyList(channel)
	storedRows, err := model.LoadChannelKeyEnvironmentRows(tx, channel.Id)
	if err != nil {
		return nil, err
	}
	byTag := make(map[string]model.ChannelKeyEnvironment, len(storedRows))
	for _, row := range storedRows {
		byTag[row.KeyTag] = row
	}

	if !setting.KeyEnvironments {
		if len(updates) > 0 {
			return nil, fmt.Errorf("该渠道未开启独立 key 环境")
		}
		present := make(map[string]bool, len(keys))
		for _, key := range keys {
			present[model.ChannelKeyTag(key)] = true
		}
		kept := make([]model.ChannelKeyEnvironment, 0, len(existing))
		for tag := range existing {
			if present[tag] {
				kept = append(kept, byTag[tag])
			}
		}
		if len(kept) == len(existing) {
			return nil, nil
		}
		return nil, model.ReplaceChannelKeyEnvironmentRows(tx, channel.Id, kept)
	}
	// Import remote-format rows once. The table wins when both formats name
	// the key. Saved legacy data stays inert until the switch is explicitly on.
	for tag, env := range channel.ChannelInfo.MultiKeyEnvs {
		if _, found := byTag[tag]; found {
			continue
		}
		row := keyEnvironmentRow(tag, env.CLIVersionProfile)
		row.Proxy = env.Proxy
		byTag[tag] = row
		existing[tag] = env.CLIVersionProfile
	}

	pool := KeyEnvironmentPool(setting.SyntheticClientHeadersProfile, setting.KeyEnvironmentCacheTTL)
	plan, err := planKeyEnvironments(keys, existing, pool, reassign)
	if err != nil {
		return nil, err
	}
	if len(updates) > 0 {
		// Editing one row must not assign other unassigned keys or re-enable
		// this key merely because its proxy changed.
		for i := range plan {
			if previous, found := existing[plan[i].tag]; found {
				plan[i].ProfileId = previous
			}
		}
	}
	for _, update := range updates {
		if update.Index < 0 || update.Index >= len(keys) {
			return nil, fmt.Errorf("密钥索引超出范围")
		}
		tag := model.ChannelKeyTag(keys[update.Index])
		for i := range plan {
			if plan[i].tag != tag {
				continue
			}
			if update.ProfileId != nil {
				id := strings.ToLower(strings.TrimSpace(*update.ProfileId))
				if id != "" {
					inPool := false
					for _, profile := range pool {
						inPool = inPool || profile.ID == id
					}
					if !inPool {
						return nil, fmt.Errorf("CLI version must belong to the channel's key environment pool and cache mode")
					}
					for _, other := range plan {
						if other.tag != tag && other.ProfileId == id {
							return nil, fmt.Errorf("CLI version is already assigned to key #%d", other.Index+1)
						}
					}
				}
				plan[i].ProfileId = id
			}
			if update.Proxy != nil {
				row := byTag[tag]
				row.Proxy = strings.TrimSpace(*update.Proxy)
				byTag[tag] = row
			}
		}
	}

	tags := make([]string, len(plan))
	for i := range plan {
		tags[i] = plan[i].tag
	}
	owners, err := model.ChannelKeyEnvironmentOwners(tx, channel.Id, tags)
	if err != nil {
		return nil, err
	}
	for _, assignment := range plan {
		ownerId, held := owners[assignment.tag]
		if !held {
			continue
		}
		conflict := &KeyEnvironmentConflictError{Index: assignment.Index, ChannelId: ownerId}
		var owner model.Channel
		if err := tx.Select("id", "name").First(&owner, ownerId).Error; err == nil {
			conflict.ChannelName = owner.Name
		}
		return nil, conflict
	}

	var unassigned []int
	rows := make([]model.ChannelKeyEnvironment, 0, len(plan))
	changed := len(plan) != len(storedRows) || len(updates) > 0 || len(channel.ChannelInfo.MultiKeyEnvs) > 0
	for _, assignment := range plan {
		if assignment.ProfileId == "" {
			unassigned = append(unassigned, assignment.Index)
		}
		if previous, ok := existing[assignment.tag]; !ok || previous != assignment.ProfileId {
			changed = true
		}
		row := keyEnvironmentRow(assignment.tag, assignment.ProfileId)
		row.Proxy = byTag[assignment.tag].Proxy
		rows = append(rows, row)
	}
	if len(unassigned) > 0 && !allowUnassigned {
		versions := make([]string, len(pool))
		for i, p := range pool {
			versions[i] = p.Version
		}
		return nil, &KeyEnvironmentShortageError{Available: len(pool), Keys: len(plan), UnassignedIndexes: unassigned, Versions: versions}
	}
	if !changed {
		return plan, nil
	}
	if err := model.ReplaceChannelKeyEnvironmentRows(tx, channel.Id, rows); err != nil {
		return nil, err
	}
	if len(channel.ChannelInfo.MultiKeyEnvs) > 0 || channel.ChannelInfo.MultiKeyOriginFingerprint != "" {
		channel.ChannelInfo.MultiKeyEnvs = nil
		channel.ChannelInfo.MultiKeyOriginFingerprint = ""
		if err := tx.Model(channel).Update("channel_info", channel.ChannelInfo).Error; err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func keyEnvironmentRows(tx *gorm.DB, channelId int) (map[string]string, error) {
	rows, err := model.LoadChannelKeyEnvironmentRows(tx, channelId)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]string, len(rows))
	for _, row := range rows {
		profileId := ""
		if row.ProfileId != nil {
			profileId = *row.ProfileId
		}
		existing[row.KeyTag] = profileId
	}
	return existing, nil
}

func keyEnvironmentRow(tag, profileId string) model.ChannelKeyEnvironment {
	row := model.ChannelKeyEnvironment{KeyTag: tag}
	if profileId != "" {
		row.ProfileId = &profileId
	}
	return row
}

// channelKeyList parses the key column as saved, ignoring a cached key slice
// that may predate the edit being reconciled.
func channelKeyList(channel *model.Channel) []string {
	if !channel.ChannelInfo.IsMultiKey {
		return []string{channel.Key}
	}
	parsed := model.Channel{Key: channel.Key}
	return parsed.GetKeys()
}

// IsProtectedProfileHeader reports whether header belongs to the CLI version
// shape of family: a header any of its captured profiles sends, one of the
// identity headers every version sets, or a session header the request shaping
// derives from the channel or key. Once a version profile is selected, overrides
// cannot replace these without changing its captured client identity.
func IsProtectedProfileHeader(family, header string) bool {
	name := strings.ToLower(strings.TrimSpace(header))
	switch {
	case name == "user-agent", name == "anthropic-beta", name == "originator",
		strings.HasPrefix(name, "x-stainless-"), strings.HasPrefix(name, "x-codex-"):
		return true
	case name == "session-id", name == "session_id", name == "thread-id", name == "thread_id",
		name == "x-client-request-id", name == "x-claude-code-session-id":
		return true
	}
	for i := range cliVersionProfiles {
		p := &cliVersionProfiles[i]
		if p.Family != family {
			continue
		}
		for profileHeader := range p.Headers {
			if strings.EqualFold(profileHeader, name) {
				return true
			}
		}
	}
	return false
}
