package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"gorm.io/gorm"
)

const (
	ResellerLifecycleNone    = "none"
	ResellerLifecycleDelete  = "delete_clients"
	ResellerLifecycleDisable = "disable_clients"
)

type resellerManagedState struct {
	AutoDisabledClientKeys []string `json:"autoDisabledClientKeys"`
}

type resellerOwnedClientTarget struct {
	InboundID int
	ClientID  string
	Email     string
	Enabled   bool
	Client    model.Client
}

func resellerClientStateKey(inboundID int, email string) string {
	return fmt.Sprintf("%d|%s", inboundID, strings.ToLower(strings.TrimSpace(email)))
}

func migrateResellerClientStateKey(tx *gorm.DB, ownerUserID, inboundID int, oldEmail, newEmail string) error {
	if tx == nil || ownerUserID <= 0 || strings.EqualFold(strings.TrimSpace(oldEmail), strings.TrimSpace(newEmail)) {
		return nil
	}
	user := &model.User{}
	if err := tx.First(user, ownerUserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	state := parseManagedState(user.ManagedState)
	oldKey := resellerClientStateKey(inboundID, oldEmail)
	newKey := resellerClientStateKey(inboundID, newEmail)
	seen := make(map[string]struct{}, len(state.AutoDisabledClientKeys))
	keys := make([]string, 0, len(state.AutoDisabledClientKeys))
	changed := false
	for _, key := range state.AutoDisabledClientKeys {
		if key == oldKey {
			key = newKey
			changed = true
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if !changed {
		return nil
	}
	sort.Strings(keys)
	return tx.Model(&model.User{}).Where("id = ?", ownerUserID).
		Update("managed_state", serializeManagedState(resellerManagedState{AutoDisabledClientKeys: keys})).Error
}

// backfillLegacyResellerOwnership upgrades comment-tag-only clients to the
// stable ownerUserId field before a reseller is renamed or lifecycle-synced.
func (s *UserService) backfillLegacyResellerOwnership(user *model.User) error {
	if user == nil || user.Id <= 0 || strings.TrimSpace(user.Username) == "" {
		return nil
	}
	db := database.GetDB()
	if db == nil {
		return fmt.Errorf("database is not initialized")
	}
	var inbounds []model.Inbound
	if err := db.Find(&inbounds).Error; err != nil {
		return err
	}
	for _, inbound := range inbounds {
		var settings map[string]json.RawMessage
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			return fmt.Errorf("decode inbound %d settings: %w", inbound.Id, err)
		}
		clientsRaw, ok := settings["clients"]
		if !ok {
			continue
		}
		var clients []json.RawMessage
		if err := json.Unmarshal(clientsRaw, &clients); err != nil {
			return fmt.Errorf("decode inbound %d clients: %w", inbound.Id, err)
		}
		changed := false
		for i, raw := range clients {
			var client model.Client
			if err := json.Unmarshal(raw, &client); err != nil {
				return fmt.Errorf("decode client %d in inbound %d: %w", i, inbound.Id, err)
			}
			if client.OwnerUserID != 0 || !ClientOwnedByReseller(client, user) {
				continue
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return fmt.Errorf("decode client fields in inbound %d: %w", inbound.Id, err)
			}
			owner, _ := json.Marshal(user.Id)
			fields["ownerUserId"] = owner
			clients[i], _ = json.Marshal(fields)
			changed = true
		}
		if !changed {
			continue
		}
		settings["clients"], _ = json.Marshal(clients)
		body, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return err
		}
		if err := db.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
			Update("settings", string(body)).Error; err != nil {
			return err
		}
	}
	return nil
}

func normalizeResellerLifecycleAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case ResellerLifecycleDelete:
		return ResellerLifecycleDelete
	case ResellerLifecycleDisable:
		return ResellerLifecycleDisable
	default:
		return ResellerLifecycleNone
	}
}

func parseManagedState(raw string) resellerManagedState {
	state := resellerManagedState{}
	if strings.TrimSpace(raw) == "" {
		return state
	}
	_ = json.Unmarshal([]byte(raw), &state)
	if len(state.AutoDisabledClientKeys) == 0 {
		return state
	}
	seen := make(map[string]struct{}, len(state.AutoDisabledClientKeys))
	normalized := make([]string, 0, len(state.AutoDisabledClientKeys))
	for _, key := range state.AutoDisabledClientKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}
	sort.Strings(normalized)
	state.AutoDisabledClientKeys = normalized
	return state
}

func serializeManagedState(state resellerManagedState) string {
	body, _ := json.Marshal(state)
	return string(body)
}

func (s *UserService) collectResellerOwnedClients(userID int) ([]resellerOwnedClientTarget, error) {
	user, err := s.GetUserByID(userID)
	if err != nil {
		return nil, err
	}
	inbounds, err := s.GetAccessibleInbounds(user)
	if err != nil {
		return nil, err
	}
	inboundService := &InboundService{}
	targets := make([]resellerOwnedClientTarget, 0)
	for _, inbound := range inbounds {
		if inbound == nil {
			continue
		}
		clients, err := inboundService.GetClients(inbound)
		if err != nil {
			return nil, err
		}
		for _, client := range clients {
			if !ClientOwnedByReseller(client, user) {
				continue
			}
			targets = append(targets, resellerOwnedClientTarget{
				InboundID: inbound.Id,
				ClientID:  multiLinkClientID(inbound.Protocol, client),
				Email:     client.Email,
				Enabled:   client.Enable,
				Client:    client,
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].InboundID == targets[j].InboundID {
			return strings.ToLower(targets[i].Email) < strings.ToLower(targets[j].Email)
		}
		return targets[i].InboundID < targets[j].InboundID
	})
	return targets, nil
}

func (s *UserService) saveManagedState(userID int, state resellerManagedState) error {
	return database.GetDB().Model(model.User{}).Where("id = ?", userID).Update("managed_state", serializeManagedState(state)).Error
}

func (s *UserService) DisableResellerOwnedClients(userID int, rememberState bool) (bool, error) {
	targets, err := s.collectResellerOwnedClients(userID)
	if err != nil {
		return false, err
	}
	user, err := s.GetUserByID(userID)
	if err != nil {
		return false, err
	}
	inboundService := &InboundService{}
	changed := false
	// Merge into the existing recorded state instead of replacing it, so
	// repeated disables (e.g. every traffic tick while over quota) never lose
	// previously recorded keys.
	state := parseManagedState(user.ManagedState)
	recorded := make(map[string]struct{}, len(state.AutoDisabledClientKeys))
	for _, key := range state.AutoDisabledClientKeys {
		recorded[key] = struct{}{}
	}
	for _, target := range targets {
		if !target.Enabled {
			continue
		}
		payloadClient := target.Client
		payloadClient.Enable = false
		// Record the lifecycle reason so later edits know this disable was
		// automatic and only the lifecycle restore may reverse it.
		payloadClient.DisableReason = DisableReasonReseller
		payloadBody, err := json.Marshal(map[string]any{"clients": []model.Client{payloadClient}})
		if err != nil {
			return changed, err
		}
		needRestart, err := inboundService.UpdateInboundClient(&model.Inbound{
			Id:       target.InboundID,
			Settings: string(payloadBody),
		}, target.ClientID)
		if err != nil {
			return changed, err
		}
		changed = changed || needRestart || target.Enabled
		if rememberState {
			key := resellerClientStateKey(target.InboundID, target.Email)
			if _, ok := recorded[key]; !ok {
				recorded[key] = struct{}{}
				keys := make([]string, 0, len(recorded))
				for recordedKey := range recorded {
					keys = append(keys, recordedKey)
				}
				sort.Strings(keys)
				state.AutoDisabledClientKeys = keys
				if err := s.saveManagedState(userID, state); err != nil {
					return changed, err
				}
			}
		}
	}
	return changed, nil
}

func (s *UserService) RestoreManagedResellerClients(userID int) (bool, error) {
	user, err := s.GetUserByID(userID)
	if err != nil {
		return false, err
	}
	state := parseManagedState(user.ManagedState)
	targets, err := s.collectResellerOwnedClients(userID)
	if err != nil {
		return false, err
	}
	allowed := make(map[string]struct{}, len(state.AutoDisabledClientKeys))
	for _, key := range state.AutoDisabledClientKeys {
		allowed[key] = struct{}{}
	}
	for _, target := range targets {
		if target.Client.DisableReason == DisableReasonReseller {
			// The client-side reason is a durable recovery marker. It also
			// recovers a lifecycle disable if a previous partial operation
			// failed before the reseller state row was saved.
			allowed[resellerClientStateKey(target.InboundID, target.Email)] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return false, nil
	}
	inboundService := &InboundService{}
	changed := false
	restored := make(map[string]struct{}, len(allowed))
	for _, target := range targets {
		key := resellerClientStateKey(target.InboundID, target.Email)
		if _, ok := allowed[key]; !ok {
			continue
		}
		if target.Enabled {
			// Already enabled again (e.g. by an admin): nothing to restore,
			// just drop it from the recorded state.
			restored[key] = struct{}{}
			continue
		}
		if target.Client.DisableReason == DisableReasonManual {
			// Manually disabled after the lifecycle disable: an admin decided
			// this client stays off. Never force-enable it; drop the key.
			restored[key] = struct{}{}
			continue
		}
		// Restore only a client whose own quota/expiry would not disable it
		// again immediately (the traffic job is authoritative for depletion).
		depleted, derr := inboundService.cohortStillDepletedForClient(target.Email, &target.Client)
		if derr != nil {
			return changed, derr
		}
		if depleted {
			// Not eligible yet: keep the key so a later sync can restore the
			// client once its quota/expiry is resolved.
			continue
		}
		payloadClient := target.Client
		payloadClient.Enable = true
		payloadClient.DisableReason = ""
		payloadBody, err := json.Marshal(map[string]any{"clients": []model.Client{payloadClient}})
		if err != nil {
			return changed, err
		}
		needRestart, err := inboundService.UpdateInboundClient(&model.Inbound{
			Id:       target.InboundID,
			Settings: string(payloadBody),
		}, target.ClientID)
		if err != nil {
			return changed, err
		}
		changed = changed || needRestart || !target.Enabled
		restored[key] = struct{}{}
	}
	// Drop recorded keys whose client no longer exists in any inbound (e.g.
	// deleted by an admin), so the recorded state cannot accumulate stale
	// entries. Keys of clients in inbounds the reseller can no longer access
	// are kept: the client still exists and may become restorable later.
	existingKeys, err := allExistingClientStateKeys()
	if err != nil {
		return changed, err
	}
	for key := range allowed {
		if _, ok := existingKeys[key]; !ok {
			restored[key] = struct{}{}
		}
	}
	remaining := make([]string, 0, len(allowed))
	for key := range allowed {
		if _, ok := restored[key]; ok {
			continue
		}
		remaining = append(remaining, key)
	}
	sort.Strings(remaining)
	if !changed && slices.Equal(remaining, state.AutoDisabledClientKeys) {
		// Nothing restored and the recorded state is unchanged: avoid a
		// database write on every sync tick.
		return changed, nil
	}
	if err := s.saveManagedState(userID, resellerManagedState{AutoDisabledClientKeys: remaining}); err != nil {
		return changed, err
	}
	return changed, nil
}

// allExistingClientStateKeys returns the resellerClientStateKey of every
// client in every inbound, regardless of reseller access. It is used to
// detect recorded lifecycle keys whose client no longer exists.
func allExistingClientStateKeys() (map[string]struct{}, error) {
	inboundService := &InboundService{}
	targets, _, err := inboundService.loadQuotaTargets(database.GetDB())
	if err != nil {
		return nil, err
	}
	keys := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		keys[resellerClientStateKey(target.InboundID, target.Email)] = struct{}{}
	}
	return keys, nil
}

func (s *UserService) DeleteResellerOwnedClients(userID int) (bool, error) {
	targets, err := s.collectResellerOwnedClients(userID)
	if err != nil {
		return false, err
	}
	inboundService := &InboundService{}
	changed := false
	seen := map[string]struct{}{}
	for _, target := range targets {
		key := resellerClientStateKey(target.InboundID, target.Email)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		needRestart, err := inboundService.DelInboundClientByEmail(target.InboundID, target.Email)
		if err != nil {
			return changed, err
		}
		changed = changed || needRestart
	}
	return changed, nil
}
