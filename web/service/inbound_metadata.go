package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database/model"
)

type inboundClientMetadata struct {
	Index int
	Raw   json.RawMessage
	Model model.Client
}

// preserveResellerClientsOnInboundUpdate keeps reseller-owned clients and
// their lifecycle metadata when an inbound is edited through a partial or
// older client payload. Client deletion remains an explicit client operation.
func preserveResellerClientsOnInboundUpdate(oldInbound, newInbound *model.Inbound) error {
	if oldInbound == nil || newInbound == nil {
		return nil
	}

	var oldSettings map[string]json.RawMessage
	if err := json.Unmarshal([]byte(oldInbound.Settings), &oldSettings); err != nil {
		return fmt.Errorf("decode existing inbound %d settings: %w", oldInbound.Id, err)
	}
	if oldSettings == nil {
		oldSettings = make(map[string]json.RawMessage)
	}
	var newSettings map[string]json.RawMessage
	if strings.TrimSpace(newInbound.Settings) == "" {
		newSettings = make(map[string]json.RawMessage)
	} else if err := json.Unmarshal([]byte(newInbound.Settings), &newSettings); err != nil {
		return fmt.Errorf("decode updated inbound %d settings: %w", oldInbound.Id, err)
	}
	if newSettings == nil {
		newSettings = make(map[string]json.RawMessage)
	}

	// Keep protocol settings omitted by a partial update.
	for key, value := range oldSettings {
		if _, exists := newSettings[key]; !exists {
			newSettings[key] = value
		}
	}

	oldClientsRaw, hasOldClients := oldSettings["clients"]
	if !hasOldClients {
		body, err := json.MarshalIndent(newSettings, "", "  ")
		if err != nil {
			return err
		}
		newInbound.Settings = string(body)
		return nil
	}
	var oldClients []json.RawMessage
	if err := json.Unmarshal(oldClientsRaw, &oldClients); err != nil {
		return fmt.Errorf("decode existing inbound %d clients: %w", oldInbound.Id, err)
	}

	nextClientsRaw, hasNextClients := newSettings["clients"]
	if !hasNextClients {
		newSettings["clients"] = oldClientsRaw
		body, err := json.MarshalIndent(newSettings, "", "  ")
		if err != nil {
			return err
		}
		newInbound.Settings = string(body)
		return nil
	}
	var nextClients []json.RawMessage
	if err := json.Unmarshal(nextClientsRaw, &nextClients); err != nil {
		return fmt.Errorf("decode updated inbound %d clients: %w", oldInbound.Id, err)
	}

	oldByKey := make(map[string]inboundClientMetadata, len(oldClients)*2)
	oldMetadata := make([]inboundClientMetadata, len(oldClients))
	for i, raw := range oldClients {
		var client model.Client
		if err := json.Unmarshal(raw, &client); err != nil {
			return fmt.Errorf("decode existing client %d in inbound %d: %w", i, oldInbound.Id, err)
		}
		metadata := inboundClientMetadata{Index: i, Raw: raw, Model: client}
		oldMetadata[i] = metadata
		for _, key := range inboundClientMatchKeys(client) {
			if _, exists := oldByKey[key]; !exists {
				oldByKey[key] = metadata
			}
		}
	}

	matchedOld := make(map[int]struct{}, len(nextClients))
	matchedNext := make(map[int]struct{}, len(nextClients))
	mergedClients := make([]json.RawMessage, 0, len(nextClients)+len(oldClients))
	for nextIndex, raw := range nextClients {
		var client model.Client
		if err := json.Unmarshal(raw, &client); err != nil {
			return fmt.Errorf("decode updated client in inbound %d: %w", oldInbound.Id, err)
		}
		var previous inboundClientMetadata
		found := false
		for _, key := range inboundClientMatchKeys(client) {
			if previous, found = oldByKey[key]; found {
				break
			}
		}
		if !found {
			mergedClients = append(mergedClients, raw)
			continue
		}
		matchedOld[previous.Index] = struct{}{}
		matchedNext[nextIndex] = struct{}{}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Errorf("decode updated client fields in inbound %d: %w", oldInbound.Id, err)
		}
		if fields == nil {
			fields = make(map[string]json.RawMessage)
		}
		if previous.Model.OwnerUserID > 0 {
			owner, _ := json.Marshal(previous.Model.OwnerUserID)
			fields["ownerUserId"] = owner
			client.OwnerUserID = previous.Model.OwnerUserID
		}
		client.Comment = mergeResellerOwnershipTags(client.Comment, previous.Model.Comment, previous.Model.OwnerUserID, client.OwnerUserID)
		comment, _ := json.Marshal(client.Comment)
		fields["comment"] = comment
		if !client.Enable && client.DisableReason == "" && previous.Model.DisableReason != "" {
			reason, _ := json.Marshal(previous.Model.DisableReason)
			fields["disableReason"] = reason
		}
		merged, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		mergedClients = append(mergedClients, merged)
	}

	// Inbound editors often retain rows while editing them. When one save
	// replaces every identity field, exact matching cannot locate its owner;
	// use the same row only if both arrays have equal length and that old row
	// has not already matched another client.
	if len(nextClients) == len(oldMetadata) {
		for i, raw := range nextClients {
			if _, alreadyMatched := matchedOld[i]; alreadyMatched {
				continue
			}
			if _, nextAlreadyMatched := matchedNext[i]; nextAlreadyMatched {
				continue
			}
			previous := oldMetadata[i]
			if previous.Model.OwnerUserID <= 0 && !resellerOwnershipTagPattern.MatchString(previous.Model.Comment) {
				continue
			}
			var nextClient model.Client
			if err := json.Unmarshal(raw, &nextClient); err != nil {
				return fmt.Errorf("decode updated client %d in inbound %d: %w", i, oldInbound.Id, err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return fmt.Errorf("decode updated client fields in inbound %d: %w", oldInbound.Id, err)
			}
			if fields == nil {
				fields = make(map[string]json.RawMessage)
			}
			if previous.Model.OwnerUserID > 0 {
				owner, _ := json.Marshal(previous.Model.OwnerUserID)
				fields["ownerUserId"] = owner
				nextClient.OwnerUserID = previous.Model.OwnerUserID
			}
			nextClient.Comment = mergeResellerOwnershipTags(nextClient.Comment, previous.Model.Comment, previous.Model.OwnerUserID, nextClient.OwnerUserID)
			comment, _ := json.Marshal(nextClient.Comment)
			fields["comment"] = comment
			if !nextClient.Enable && nextClient.DisableReason == "" && previous.Model.DisableReason != "" {
				reason, _ := json.Marshal(previous.Model.DisableReason)
				fields["disableReason"] = reason
			}
			merged, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			mergedClients[i] = merged
			matchedOld[i] = struct{}{}
			matchedNext[i] = struct{}{}
		}
	}

	// A full inbound save is not a client deletion API. If a reseller-owned
	// client was omitted, carry it forward so other inbound edits cannot make
	// that reseller lose visibility or management of the client.
	for _, previous := range oldMetadata {
		if _, matched := matchedOld[previous.Index]; matched {
			continue
		}
		if previous.Model.OwnerUserID > 0 || resellerOwnershipTagPattern.MatchString(previous.Model.Comment) {
			mergedClients = append(mergedClients, previous.Raw)
		}
	}

	clientsJSON, err := json.Marshal(mergedClients)
	if err != nil {
		return err
	}
	newSettings["clients"] = clientsJSON
	body, err := json.MarshalIndent(newSettings, "", "  ")
	if err != nil {
		return err
	}
	newInbound.Settings = string(body)
	return nil
}

func inboundClientMatchKeys(client model.Client) []string {
	keys := make([]string, 0, 5)
	add := func(prefix, value string, fold bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if fold {
			value = strings.ToLower(value)
		}
		keys = append(keys, prefix+value)
	}
	add("email:", client.Email, true)
	add("id:", client.ID, false)
	add("password:", client.Password, false)
	add("auth:", client.Auth, false)
	add("subid:", client.SubID, true)
	return keys
}
