// Package service provides business logic services for the 3x-ui web panel,
// including inbound/outbound management, user administration, settings, and Xray integration.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/common"
	"github.com/mhsanaei/3x-ui/v2/util/random"
	"github.com/mhsanaei/3x-ui/v2/xray"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// InboundService provides business logic for managing Xray inbound configurations.
// It handles CRUD operations for inbounds, client management, traffic monitoring,
// and integration with the Xray API for real-time updates.
type InboundService struct {
	xrayApi xray.XrayAPI
}

type quotaTarget struct {
	InboundID      int
	Tag            string
	NodeID         *int
	Email          string
	SubID          string
	TotalGB        int64
	NominalTotalGB int64
	QuotaFactor    *int64
	ExpiryTime     int64
	ClientEnable   bool
}

type quotaCohort struct {
	Key      string
	Emails   map[string]struct{}
	Targets  []quotaTarget
	Used     int64
	Limit    int64
	Expired  bool
	Depleted bool
}

// Disable reasons recorded on a disabled client (model.Client.DisableReason).
// The empty string means an enabled client, or a client disabled before the
// marker existed (legacy data), for which callers fall back to a heuristic.
const (
	DisableReasonManual   = "manual"   // An admin explicitly disabled the client
	DisableReasonQuota    = "quota"    // The traffic job disabled it: volume exhausted
	DisableReasonExpiry   = "expiry"   // The traffic job disabled it: expired
	DisableReasonReseller = "reseller" // The reseller lifecycle disabled it
)

func normalizeInboundNodeTarget(inbound *model.Inbound) {
	if inbound == nil {
		return
	}
	if inbound.NodeID != nil && *inbound.NodeID <= 0 {
		inbound.NodeID = nil
	}
	if inbound.NodeInboundID != nil && *inbound.NodeInboundID <= 0 {
		inbound.NodeInboundID = nil
	}
}

func normalizeMultiInboundIDs(primaryID int, selected []int) []int {
	seen := map[int]struct{}{}
	out := make([]int, 0, len(selected)+1)
	if primaryID > 0 {
		seen[primaryID] = struct{}{}
		out = append(out, primaryID)
	}
	for _, id := range selected {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func cloneClientForProtocol(protocol model.Protocol, template model.Client) model.Client {
	c := template
	switch protocol {
	case model.VMESS, model.VLESS:
		if strings.TrimSpace(c.ID) == "" {
			c.ID = uuid.NewString()
		}
	case model.Trojan:
		if strings.TrimSpace(c.Password) == "" {
			c.Password = random.Seq(12)
		}
	case model.Shadowsocks:
		if strings.TrimSpace(c.Password) == "" {
			c.Password = random.Seq(16)
		}
	case model.Hysteria:
		if strings.TrimSpace(c.Auth) == "" {
			c.Auth = random.Seq(12)
		}
	}
	return c
}

func sanitizeEmailPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "client"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "client"
	}
	return out
}

func (s *InboundService) allocateMultiLinkEmail(baseEmail string, inboundID int, used map[string]struct{}) string {
	base := sanitizeEmailPart(baseEmail)
	candidate := fmt.Sprintf("%s-ml-%d", base, inboundID)
	if _, ok := used[candidate]; !ok {
		used[candidate] = struct{}{}
		return candidate
	}
	for i := 2; i < 1000; i++ {
		candidate = fmt.Sprintf("%s-ml-%d-%d", base, inboundID, i)
		if _, ok := used[candidate]; !ok {
			used[candidate] = struct{}{}
			return candidate
		}
	}
	candidate = fmt.Sprintf("%s-ml-%d-%s", base, inboundID, random.Seq(6))
	used[candidate] = struct{}{}
	return candidate
}

func (s *InboundService) SetUserRuntimeEnabled(userID int, enabled bool) (bool, error) {
	defer ClientLimitGateRefresh()
	userService := &UserService{}
	user, err := userService.GetUserByID(userID)
	if err != nil {
		return false, err
	}
	inbounds, err := userService.GetAccessibleInbounds(user)
	if err != nil {
		return false, err
	}
	db := database.GetDB()
	changed := false
	isReseller := userService.IsReseller(user)
	for _, inbound := range inbounds {
		if !isReseller && inbound.Enable != enabled {
			changed = true
		}
		if !isReseller {
			if err := db.Model(model.Inbound{}).Where("id = ?", inbound.Id).Update("enable", enabled).Error; err != nil {
				return changed, err
			}
		}
		clients, err := s.GetClients(inbound)
		if err != nil {
			return changed, err
		}
		inboundChanged := false
		for i := range clients {
			if isReseller && !ClientOwnedByReseller(clients[i], user) {
				continue
			}
			if clients[i].Enable != enabled {
				changed = true
				inboundChanged = true
			}
			clients[i].Enable = enabled
		}
		if !inboundChanged {
			continue
		}
		// Preserve the rest of the settings object (protocol-level keys such
		// as shadowsocks method/password or vless fallbacks); only replace the
		// clients key.
		settings := map[string]any{}
		if len(inbound.Settings) > 0 {
			if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
				return changed, err
			}
		}
		body, err := json.Marshal(clients)
		if err != nil {
			return changed, err
		}
		clientList := []any{}
		if err := json.Unmarshal(body, &clientList); err != nil {
			return changed, err
		}
		settings["clients"] = clientList
		full, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return changed, err
		}
		if err := db.Model(model.Inbound{}).Where("id = ?", inbound.Id).Update("settings", string(full)).Error; err != nil {
			return changed, err
		}
		// Enable state flipped for these clients: refresh their cached IP
		// limit policy so the engine reacts immediately.
		for i := range clients {
			clientLimitEngineNotifyClientUpdated(clients[i].Email, clients[i].Email)
		}
	}
	return changed, nil
}

func (s *InboundService) getInboundNode(inbound *model.Inbound) (*model.Node, int, error) {
	normalizeInboundNodeTarget(inbound)
	if inbound == nil || inbound.NodeID == nil {
		return nil, 0, nil
	}
	if inbound.NodeInboundID == nil {
		return nil, 0, common.NewError("node inbound mapping is missing")
	}
	nodeService := NodeService{}
	n, err := nodeService.GetById(*inbound.NodeID)
	if err != nil {
		return nil, 0, err
	}
	return n, *inbound.NodeInboundID, nil
}

func (s *InboundService) loadQuotaTargets(tx *gorm.DB) ([]quotaTarget, map[string]int64, error) {
	var inbounds []*model.Inbound
	if err := tx.Model(model.Inbound{}).Find(&inbounds).Error; err != nil {
		return nil, nil, err
	}

	targets := make([]quotaTarget, 0)
	for _, inbound := range inbounds {
		clients, err := s.GetClients(inbound)
		if err != nil {
			continue
		}
		for _, client := range clients {
			email := strings.TrimSpace(strings.ToLower(client.Email))
			if email == "" {
				continue
			}
			targets = append(targets, quotaTarget{
				InboundID:      inbound.Id,
				Tag:            inbound.Tag,
				NodeID:         inbound.NodeID,
				Email:          email,
				SubID:          strings.TrimSpace(client.SubID),
				NominalTotalGB: client.TotalGB,
				QuotaFactor:    client.QuotaFactor,
				ExpiryTime:     client.ExpiryTime,
				ClientEnable:   client.Enable,
			})
		}
	}

	var traffics []*xray.ClientTraffic
	if err := tx.Model(xray.ClientTraffic{}).Find(&traffics).Error; err != nil {
		return nil, nil, err
	}
	usageByEmail := make(map[string]int64, len(traffics))
	trafficByEmail := make(map[string]*xray.ClientTraffic, len(traffics))
	for _, traffic := range traffics {
		email := strings.TrimSpace(strings.ToLower(traffic.Email))
		if email == "" {
			continue
		}
		usageByEmail[email] = traffic.Up + traffic.Down
		trafficByEmail[email] = traffic
	}
	for i := range targets {
		client := model.Client{TotalGB: targets[i].NominalTotalGB, QuotaFactor: targets[i].QuotaFactor}
		targets[i].TotalGB = model.InitialClientTrafficQuota(client)
		if traffic := trafficByEmail[strings.ToLower(strings.TrimSpace(targets[i].Email))]; traffic != nil && traffic.Total > 0 {
			targets[i].TotalGB = traffic.Total
		}
	}
	return targets, usageByEmail, nil
}

func (s *InboundService) depletedQuotaCohorts(tx *gorm.DB, now int64) (map[string]*quotaCohort, error) {
	targets, usageByEmail, err := s.loadQuotaTargets(tx)
	if err != nil {
		return nil, err
	}
	cohorts := make(map[string]*quotaCohort)
	for _, target := range targets {
		key := target.Email
		if target.SubID != "" {
			key = "sub:" + target.SubID
		}
		cohort, ok := cohorts[key]
		if !ok {
			cohort = &quotaCohort{
				Key:    key,
				Emails: map[string]struct{}{},
				Limit:  0,
			}
			cohorts[key] = cohort
		}
		cohort.Targets = append(cohort.Targets, target)
		cohort.Emails[target.Email] = struct{}{}
		cohort.Used += usageByEmail[target.Email]
		if target.ClientEnable && target.ExpiryTime > 0 && target.ExpiryTime <= now {
			cohort.Expired = true
		}
		if target.TotalGB > 0 && target.TotalGB > cohort.Limit {
			cohort.Limit = target.TotalGB
		}
	}
	for _, cohort := range cohorts {
		if cohort.Expired {
			cohort.Depleted = true
			continue
		}
		if cohort.Limit > 0 && cohort.Used >= cohort.Limit {
			cohort.Depleted = true
		}
	}
	return cohorts, nil
}

// setClientsDisabledInInbound disables the given clients inside an inbound's
// settings blob and records why (email -> DisableReasonQuota/Expiry), so a
// later edit can tell an automatic disable from a manual one. Clients that are
// already disabled keep their existing reason: a manual disable is never
// overwritten by the traffic job.
func (s *InboundService) setClientsDisabledInInbound(tx *gorm.DB, inbound *model.Inbound, emailReasons map[string]string) error {
	if inbound == nil || len(emailReasons) == 0 {
		return nil
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return err
	}
	rawClients, ok := settings["clients"].([]any)
	if !ok || len(rawClients) == 0 {
		return nil
	}
	changed := false
	for _, client := range rawClients {
		cm, ok := client.(map[string]any)
		if !ok {
			continue
		}
		email, _ := cm["email"].(string)
		reason, match := emailReasons[strings.ToLower(strings.TrimSpace(email))]
		if !match {
			continue
		}
		if current, exists := cm["enable"].(bool); exists && !current {
			// Already disabled (possibly manually): keep its recorded reason.
			continue
		}
		cm["enable"] = false
		if reason != "" {
			cm["disableReason"] = reason
		}
		changed = true
	}
	if !changed {
		return nil
	}
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := tx.Model(model.Inbound{}).Where("id = ?", inbound.Id).Update("settings", string(body)).Error; err != nil {
		return err
	}
	// Disabled clients must stop counting against their IP limit at once.
	for email := range emailReasons {
		clientLimitEngineNotifyClientUpdated(email, email)
	}
	return nil
}

// GetInbounds retrieves all inbounds for a specific user.
// Returns a slice of inbound models with their associated client statistics.
func (s *InboundService) GetInbounds(userId int) ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Preload("ClientStats").Where("user_id = ?", userId).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	// Enrich client stats with UUID/SubId from inbound settings
	for _, inbound := range inbounds {
		clients, _ := s.GetClients(inbound)
		if len(clients) == 0 || len(inbound.ClientStats) == 0 {
			continue
		}
		// Build a map email -> client
		cMap := make(map[string]model.Client, len(clients))
		for _, c := range clients {
			cMap[strings.ToLower(c.Email)] = c
		}
		for i := range inbound.ClientStats {
			email := strings.ToLower(inbound.ClientStats[i].Email)
			if c, ok := cMap[email]; ok {
				inbound.ClientStats[i].UUID = c.ID
				inbound.ClientStats[i].SubId = c.SubID
			}
		}
	}
	return inbounds, nil
}

// GetAllInbounds retrieves all inbounds from the database.
// Returns a slice of all inbound models with their associated client statistics.
func (s *InboundService) GetAllInbounds() ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Preload("ClientStats").Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	// Enrich client stats with UUID/SubId from inbound settings
	for _, inbound := range inbounds {
		clients, _ := s.GetClients(inbound)
		if len(clients) == 0 || len(inbound.ClientStats) == 0 {
			continue
		}
		cMap := make(map[string]model.Client, len(clients))
		for _, c := range clients {
			cMap[strings.ToLower(c.Email)] = c
		}
		for i := range inbound.ClientStats {
			email := strings.ToLower(inbound.ClientStats[i].Email)
			if c, ok := cMap[email]; ok {
				inbound.ClientStats[i].UUID = c.ID
				inbound.ClientStats[i].SubId = c.SubID
			}
		}
	}
	return inbounds, nil
}

func (s *InboundService) GetInboundsByTrafficReset(period string) ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Where("traffic_reset = ?", period).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return inbounds, nil
}

func (s *InboundService) checkPortExist(listen string, port int, ignoreId int) (bool, error) {
	db := database.GetDB()
	if listen == "" || listen == "0.0.0.0" || listen == "::" || listen == "::0" {
		db = db.Model(model.Inbound{}).Where("port = ?", port)
	} else {
		db = db.Model(model.Inbound{}).
			Where("port = ?", port).
			Where(
				db.Model(model.Inbound{}).Where(
					"listen = ?", listen,
				).Or(
					"listen = \"\"",
				).Or(
					"listen = \"0.0.0.0\"",
				).Or(
					"listen = \"::\"",
				).Or(
					"listen = \"::0\""))
	}
	if ignoreId > 0 {
		db = db.Where("id != ?", ignoreId)
	}
	var count int64
	err := db.Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *InboundService) GetClients(inbound *model.Inbound) ([]model.Client, error) {
	if inbound == nil {
		return nil, fmt.Errorf("inbound is nil")
	}
	// A struct (not map[string][]model.Client) so extra top-level keys like
	// vless "decryption" don't make the unmarshal fail; they are ignored.
	var settings struct {
		Clients []model.Client `json:"clients"`
	}
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return nil, fmt.Errorf("invalid settings JSON for inbound %d: %w", inbound.Id, err)
	}
	if settings.Clients == nil {
		return nil, nil
	}
	return settings.Clients, nil
}

// inboundSettingsObject decodes a stored or submitted settings blob without
// leaving callers with a nil map for JSON `null`. Older/imported records can
// contain that value; callers that add the first client can safely initialize
// the object, while callers that require an existing client get an empty list.
func inboundSettingsObject(raw string, inboundID int) (map[string]any, error) {
	settings := make(map[string]any)
	if strings.TrimSpace(raw) == "" {
		return settings, nil
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return nil, fmt.Errorf("invalid settings JSON for inbound %d: %w", inboundID, err)
	}
	if settings == nil {
		settings = make(map[string]any)
	}
	return settings, nil
}

// inboundSettingsClients validates the untyped client list used by the legacy
// settings mutation paths. Missing/null clients mean an empty list; malformed
// rows return an error instead of panicking on a type assertion.
func inboundSettingsClients(settings map[string]any, inboundID int) ([]any, error) {
	if settings == nil {
		return nil, fmt.Errorf("settings for inbound %d must be a JSON object", inboundID)
	}
	raw, exists := settings["clients"]
	if !exists || raw == nil {
		return []any{}, nil
	}
	clients, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("clients for inbound %d must be a JSON array", inboundID)
	}
	for index, client := range clients {
		if _, ok := client.(map[string]any); !ok {
			return nil, fmt.Errorf("client %d for inbound %d must be a JSON object", index, inboundID)
		}
	}
	return clients, nil
}

func validateClientIPLimits(clients []model.Client) error {
	for _, client := range clients {
		if !model.ValidateClientQuotaFactor(client.QuotaFactor) {
			return fmt.Errorf("traffic coefficient for client %q must be between 0 and %d", client.Email, model.MaxClientQuotaFactor)
		}
		if client.LimitIP < 0 {
			return fmt.Errorf("IP limit cannot be negative for client %q", client.Email)
		}
		if client.LimitIP > 0 && strings.TrimSpace(client.Email) == "" {
			return fmt.Errorf("a non-empty email is required to enforce an IP limit")
		}
	}
	return nil
}

func (s *InboundService) getAllEmails() ([]string, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	if err := db.Model(model.Inbound{}).Select("settings").Find(&inbounds).Error; err != nil {
		return nil, err
	}
	emails := make([]string, 0)
	for _, inbound := range inbounds {
		clients, err := s.GetClients(inbound)
		if err != nil {
			return nil, err
		}
		for _, client := range clients {
			if client.Email != "" {
				emails = append(emails, client.Email)
			}
		}
	}
	if len(emails) == 0 {
		return []string{}, nil
	}
	return emails, nil
}

func (s *InboundService) contains(slice []string, str string) bool {
	lowerStr := strings.ToLower(str)
	for _, s := range slice {
		if strings.ToLower(s) == lowerStr {
			return true
		}
	}
	return false
}

func (s *InboundService) checkEmailsExistForClients(clients []model.Client) (string, error) {
	allEmails, err := s.getAllEmails()
	if err != nil {
		return "", err
	}
	var emails []string
	for _, client := range clients {
		if client.Email != "" {
			if s.contains(emails, client.Email) {
				return client.Email, nil
			}
			if s.contains(allEmails, client.Email) {
				return client.Email, nil
			}
			emails = append(emails, client.Email)
		}
	}
	return "", nil
}

func (s *InboundService) checkEmailExistForInbound(inbound *model.Inbound) (string, error) {
	clients, err := s.GetClients(inbound)
	if err != nil {
		return "", err
	}
	allEmails, err := s.getAllEmails()
	if err != nil {
		return "", err
	}
	var emails []string
	for _, client := range clients {
		if client.Email != "" {
			if s.contains(emails, client.Email) {
				return client.Email, nil
			}
			if s.contains(allEmails, client.Email) {
				return client.Email, nil
			}
			emails = append(emails, client.Email)
		}
	}
	return "", nil
}

// AddInbound creates a new inbound configuration.
// It validates port uniqueness, client email uniqueness, and required fields,
// then saves the inbound to the database and optionally adds it to the running Xray instance.
// Returns the created inbound, whether Xray needs restart, and any error.
func (s *InboundService) AddInbound(inbound *model.Inbound) (*model.Inbound, bool, error) {
	defer ClientLimitGateRefresh()
	normalizedSettings, _, err := normalizeVLESSDecryption(string(inbound.Protocol), inbound.Settings)
	if err != nil {
		return inbound, false, err
	}
	inbound.Settings = normalizedSettings
	normalizeInboundNodeTarget(inbound)
	if inbound.NodeID == nil {
		exist, err := s.checkPortExist(inbound.Listen, inbound.Port, 0)
		if err != nil {
			return inbound, false, err
		}
		if exist {
			return inbound, false, common.NewError("Port already exists:", inbound.Port)
		}
	}

	existEmail, err := s.checkEmailExistForInbound(inbound)
	if err != nil {
		return inbound, false, err
	}
	if existEmail != "" {
		return inbound, false, common.NewError("Duplicate email:", existEmail)
	}

	clients, err := s.GetClients(inbound)
	if err != nil {
		return inbound, false, err
	}
	if err := validateClientIPLimits(clients); err != nil {
		return inbound, false, err
	}

	// Ensure created_at and updated_at on clients in settings
	if len(clients) > 0 {
		var settings map[string]any
		if err2 := json.Unmarshal([]byte(inbound.Settings), &settings); err2 == nil && settings != nil {
			now := time.Now().Unix() * 1000
			updatedClients := make([]model.Client, 0, len(clients))
			for _, c := range clients {
				if c.CreatedAt == 0 {
					c.CreatedAt = now
				}
				c.UpdatedAt = now
				updatedClients = append(updatedClients, c)
			}
			settings["clients"] = updatedClients
			if bs, err3 := json.MarshalIndent(settings, "", "  "); err3 == nil {
				inbound.Settings = string(bs)
			} else {
				logger.Debug("Unable to marshal inbound settings with timestamps:", err3)
			}
		} else if err2 != nil {
			logger.Debug("Unable to parse inbound settings for timestamps:", err2)
		}
	}

	// Secure client ID
	for _, client := range clients {
		switch inbound.Protocol {
		case "trojan":
			if client.Password == "" {
				return inbound, false, common.NewError("empty client ID")
			}
		case "shadowsocks":
			if client.Email == "" {
				return inbound, false, common.NewError("empty client ID")
			}
		case "hysteria":
			if client.Auth == "" {
				return inbound, false, common.NewError("empty client ID")
			}
		default:
			if client.ID == "" {
				return inbound, false, common.NewError("empty client ID")
			}
		}
	}

	if inbound.NodeID != nil {
		nodeService := NodeService{}
		node, err := nodeService.GetById(*inbound.NodeID)
		if err != nil {
			return inbound, false, err
		}
		if err := nodeService.normalize(node); err != nil {
			return inbound, false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		created, err := nodeService.CreateRemoteInbound(ctx, node, inbound)
		if err != nil {
			return inbound, false, err
		}
		local := *inbound
		remoteID := created.Id
		local.NodeInboundID = &remoteID
		local.Up = created.Up
		local.Down = created.Down
		local.AllTime = created.AllTime
		if created.Tag != "" {
			local.Tag = created.Tag
		}
		db := database.GetDB()
		tx := db.Begin()
		if tx.Error != nil {
			return inbound, false, tx.Error
		}
		if err := tx.Create(&local).Error; err != nil {
			tx.Rollback()
			return inbound, false, err
		}
		for _, client := range clients {
			if err := s.AddClientStat(tx, local.Id, &client); err != nil {
				tx.Rollback()
				return inbound, false, err
			}
		}
		if err := tx.Commit().Error; err != nil {
			return inbound, false, err
		}
		inbound = &local
		_, _ = nodeService.SyncNode(ctx, node.Id)
		return inbound, false, nil
	}

	db := database.GetDB()
	tx := db.Begin()
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()

	err = tx.Save(inbound).Error
	if err == nil {
		if len(inbound.ClientStats) == 0 {
			for _, client := range clients {
				s.AddClientStat(tx, inbound.Id, &client)
			}
		}
	} else {
		return inbound, false, err
	}

	needRestart := false
	if inbound.Enable {
		s.xrayApi.Init(p.GetAPIPort())
		inboundJson, err1 := json.MarshalIndent(inbound.GenXrayInboundConfig(), "", "  ")
		if err1 != nil {
			logger.Debug("Unable to marshal inbound config:", err1)
		}

		err1 = s.xrayApi.AddInbound(inboundJson)
		if err1 == nil {
			logger.Debug("New inbound added by api:", inbound.Tag)
		} else {
			logger.Debug("Unable to add inbound by api:", err1)
			needRestart = true
		}
		s.xrayApi.Close()
	}

	return inbound, needRestart, err
}

// DelInbound deletes an inbound configuration by ID.
// It removes the inbound from the database and the running Xray instance if active.
// Returns whether Xray needs restart and any error.
func (s *InboundService) DelInbound(id int) (bool, error) {
	defer ClientLimitGateRefresh()
	db := database.GetDB()
	inbound, err := s.GetInbound(id)
	if err != nil {
		return false, err
	}

	if inbound.NodeID != nil {
		node, remoteID, err := s.getInboundNode(inbound)
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := (&NodeService{}).DeleteRemoteInbound(ctx, node, remoteID); err != nil {
			return false, err
		}
	}

	var tag string
	needRestart := false
	result := db.Model(model.Inbound{}).Select("tag").Where("id = ? and enable = ?", id, true).First(&tag)
	if inbound.NodeID == nil && result.Error == nil {
		s.xrayApi.Init(p.GetAPIPort())
		err1 := s.xrayApi.DelInbound(tag)
		if err1 == nil {
			logger.Debug("Inbound deleted by api:", tag)
		} else {
			logger.Debug("Unable to delete inbound by api:", err1)
			needRestart = true
		}
		s.xrayApi.Close()
	} else {
		logger.Debug("No enabled inbound founded to removing by api", tag)
	}

	// Delete client traffics of inbounds
	err = db.Where("inbound_id = ?", id).Delete(xray.ClientTraffic{}).Error
	if err != nil {
		return false, err
	}
	clients, err := s.GetClients(inbound)
	if err != nil {
		return false, err
	}
	for _, client := range clients {
		err := s.DelClientIPs(db, client.Email)
		if err != nil {
			return false, err
		}
	}

	return needRestart, db.Delete(model.Inbound{}, id).Error
}

func (s *InboundService) GetInbound(id int) (*model.Inbound, error) {
	db := database.GetDB()
	inbound := &model.Inbound{}
	err := db.Model(model.Inbound{}).First(inbound, id).Error
	if err != nil {
		return nil, err
	}
	return inbound, nil
}

// UpdateInbound modifies an existing inbound configuration.
// It validates changes, updates the database, and syncs with the running Xray instance.
// Returns the updated inbound, whether Xray needs restart, and any error.
func (s *InboundService) UpdateInbound(inbound *model.Inbound) (*model.Inbound, bool, error) {
	defer ClientLimitGateRefresh()
	normalizeInboundNodeTarget(inbound)
	oldInbound, err := s.GetInbound(inbound.Id)
	if err != nil {
		return inbound, false, err
	}
	if inbound.Protocol == "" {
		inbound.Protocol = oldInbound.Protocol
	}
	normalizedSettings, _, err := normalizeVLESSDecryption(string(inbound.Protocol), inbound.Settings)
	if err != nil {
		return inbound, false, err
	}
	inbound.Settings = normalizedSettings
	if err := preserveResellerClientsOnInboundUpdate(oldInbound, inbound); err != nil {
		return inbound, false, err
	}
	updatedClients, err := s.GetClients(inbound)
	if err != nil {
		return inbound, false, err
	}
	if err := validateClientIPLimits(updatedClients); err != nil {
		return inbound, false, err
	}
	if oldInbound.NodeID == nil {
		exist, err := s.checkPortExist(inbound.Listen, inbound.Port, inbound.Id)
		if err != nil {
			return inbound, false, err
		}
		if exist {
			return inbound, false, common.NewError("Port already exists:", inbound.Port)
		}
	}

	if oldInbound.NodeID != nil {
		node, remoteID, err := s.getInboundNode(oldInbound)
		if err != nil {
			return inbound, false, err
		}
		inbound.NodeID = oldInbound.NodeID
		inbound.NodeInboundID = oldInbound.NodeInboundID
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		updated, err := (&NodeService{}).UpdateRemoteInbound(ctx, node, remoteID, inbound)
		if err != nil {
			return inbound, false, err
		}
		oldInbound.Up = updated.Up
		oldInbound.Down = updated.Down
		oldInbound.AllTime = updated.AllTime
		oldInbound.Total = inbound.Total
		oldInbound.Remark = inbound.Remark
		oldInbound.Enable = inbound.Enable
		oldInbound.ExpiryTime = inbound.ExpiryTime
		oldInbound.TrafficReset = inbound.TrafficReset
		oldInbound.Listen = inbound.Listen
		oldInbound.Port = inbound.Port
		oldInbound.Protocol = inbound.Protocol
		oldInbound.Settings = inbound.Settings
		oldInbound.StreamSettings = inbound.StreamSettings
		oldInbound.Sniffing = inbound.Sniffing
		if updated.Tag != "" {
			oldInbound.Tag = updated.Tag
		}
		if err := database.GetDB().Save(oldInbound).Error; err != nil {
			return inbound, false, err
		}
		_, _ = (&NodeService{}).SyncNode(ctx, node.Id)
		return oldInbound, false, nil
	}

	tag := oldInbound.Tag

	db := database.GetDB()
	tx := db.Begin()

	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	err = s.updateClientTraffics(tx, oldInbound, inbound)
	if err != nil {
		return inbound, false, err
	}

	// Ensure created_at and updated_at exist in inbound.Settings clients
	{
		var oldSettings map[string]any
		_ = json.Unmarshal([]byte(oldInbound.Settings), &oldSettings)
		emailToCreated := map[string]int64{}
		emailToUpdated := map[string]int64{}
		if oldSettings != nil {
			if oc, ok := oldSettings["clients"].([]any); ok {
				for _, it := range oc {
					if m, ok2 := it.(map[string]any); ok2 {
						if email, ok3 := m["email"].(string); ok3 {
							switch v := m["created_at"].(type) {
							case float64:
								emailToCreated[email] = int64(v)
							case int64:
								emailToCreated[email] = v
							}
							switch v := m["updated_at"].(type) {
							case float64:
								emailToUpdated[email] = int64(v)
							case int64:
								emailToUpdated[email] = v
							}
						}
					}
				}
			}
		}
		var newSettings map[string]any
		if err2 := json.Unmarshal([]byte(inbound.Settings), &newSettings); err2 == nil && newSettings != nil {
			now := time.Now().Unix() * 1000
			if nSlice, ok := newSettings["clients"].([]any); ok {
				for i := range nSlice {
					if m, ok2 := nSlice[i].(map[string]any); ok2 {
						email, _ := m["email"].(string)
						if _, ok3 := m["created_at"]; !ok3 {
							if v, ok4 := emailToCreated[email]; ok4 && v > 0 {
								m["created_at"] = v
							} else {
								m["created_at"] = now
							}
						}
						// Preserve client's updated_at if present; do not bump on parent inbound update
						if _, hasUpdated := m["updated_at"]; !hasUpdated {
							if v, ok4 := emailToUpdated[email]; ok4 && v > 0 {
								m["updated_at"] = v
							}
						}
						nSlice[i] = m
					}
				}
				newSettings["clients"] = nSlice
				if bs, err3 := json.MarshalIndent(newSettings, "", "  "); err3 == nil {
					inbound.Settings = string(bs)
				}
			}
		}
	}

	oldInbound.Up = inbound.Up
	oldInbound.Down = inbound.Down
	oldInbound.Total = inbound.Total
	oldInbound.Remark = inbound.Remark
	oldInbound.Enable = inbound.Enable
	oldInbound.ExpiryTime = inbound.ExpiryTime
	oldInbound.TrafficReset = inbound.TrafficReset
	oldInbound.Listen = inbound.Listen
	oldInbound.Port = inbound.Port
	oldInbound.Protocol = inbound.Protocol
	oldInbound.Settings = inbound.Settings
	oldInbound.StreamSettings = inbound.StreamSettings
	oldInbound.Sniffing = inbound.Sniffing
	if inbound.Listen == "" || inbound.Listen == "0.0.0.0" || inbound.Listen == "::" || inbound.Listen == "::0" {
		oldInbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)
	} else {
		oldInbound.Tag = fmt.Sprintf("inbound-%v:%v", inbound.Listen, inbound.Port)
	}

	needRestart := false
	s.xrayApi.Init(p.GetAPIPort())
	if s.xrayApi.DelInbound(tag) == nil {
		logger.Debug("Old inbound deleted by api:", tag)
	}
	if inbound.Enable {
		runtimeInbound, err2 := s.buildRuntimeInboundForAPI(tx, oldInbound)
		if err2 != nil {
			logger.Debug("Unable to prepare runtime inbound config:", err2)
			needRestart = true
		} else {
			inboundJson, err2 := json.MarshalIndent(runtimeInbound.GenXrayInboundConfig(), "", "  ")
			if err2 != nil {
				logger.Debug("Unable to marshal updated inbound config:", err2)
				needRestart = true
			} else {
				err2 = s.xrayApi.AddInbound(inboundJson)
				if err2 == nil {
					logger.Debug("Updated inbound added by api:", oldInbound.Tag)
				} else {
					logger.Debug("Unable to update inbound by api:", err2)
					needRestart = true
				}
			}
		}
	}
	s.xrayApi.Close()

	err = tx.Save(oldInbound).Error
	return inbound, needRestart, err
}

func (s *InboundService) buildRuntimeInboundForAPI(tx *gorm.DB, inbound *model.Inbound) (*model.Inbound, error) {
	if inbound == nil {
		return nil, fmt.Errorf("inbound is nil")
	}

	runtimeInbound := *inbound
	normalizedSettings, _, err := normalizeVLESSDecryption(string(runtimeInbound.Protocol), runtimeInbound.Settings)
	if err != nil {
		return nil, err
	}
	runtimeInbound.Settings = normalizedSettings
	settings := map[string]any{}
	if err := json.Unmarshal([]byte(runtimeInbound.Settings), &settings); err != nil {
		return nil, err
	}

	clients, ok := settings["clients"].([]any)
	if !ok {
		return &runtimeInbound, nil
	}

	var clientStats []xray.ClientTraffic
	err = tx.Model(xray.ClientTraffic{}).
		Where("inbound_id = ?", inbound.Id).
		Select("email", "enable").
		Find(&clientStats).Error
	if err != nil {
		return nil, err
	}

	enableMap := make(map[string]bool, len(clientStats))
	for _, clientTraffic := range clientStats {
		enableMap[clientTraffic.Email] = clientTraffic.Enable
	}

	finalClients := make([]any, 0, len(clients))
	for _, client := range clients {
		c, ok := client.(map[string]any)
		if !ok {
			continue
		}

		email, _ := c["email"].(string)
		if enable, exists := enableMap[email]; exists && !enable {
			continue
		}

		if manualEnable, ok := c["enable"].(bool); ok && !manualEnable {
			continue
		}

		finalClients = append(finalClients, c)
	}

	settings["clients"] = finalClients
	modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	runtimeInbound.Settings = string(modifiedSettings)

	return &runtimeInbound, nil
}

func (s *InboundService) updateClientTraffics(tx *gorm.DB, oldInbound *model.Inbound, newInbound *model.Inbound) error {
	oldClients, err := s.GetClients(oldInbound)
	if err != nil {
		return err
	}
	newClients, err := s.GetClients(newInbound)
	if err != nil {
		return err
	}

	var emailExists bool

	for _, oldClient := range oldClients {
		emailExists = false
		for _, newClient := range newClients {
			if oldClient.Email == newClient.Email {
				emailExists = true
				break
			}
		}
		if !emailExists {
			err = s.DelClientStat(tx, oldClient.Email)
			if err != nil {
				return err
			}
		}
	}
	for _, newClient := range newClients {
		emailExists = false
		for _, oldClient := range oldClients {
			if newClient.Email == oldClient.Email {
				emailExists = true
				break
			}
		}
		if !emailExists {
			err = s.AddClientStat(tx, oldInbound.Id, &newClient)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *InboundService) AddInboundClient(data *model.Inbound) (bool, error) {
	defer ClientLimitGateRefresh()
	clients, err := s.GetClients(data)
	if err != nil {
		return false, err
	}
	if len(clients) == 0 {
		return false, common.NewError("no client payload provided")
	}
	if err := validateClientIPLimits(clients); err != nil {
		return false, err
	}

	settings, err := inboundSettingsObject(data.Settings, data.Id)
	if err != nil {
		return false, err
	}

	interfaceClients, err := inboundSettingsClients(settings, data.Id)
	if err != nil {
		return false, err
	}
	if len(interfaceClients) == 0 {
		return false, common.NewError("invalid clients list in request settings")
	}
	// Add timestamps for new clients being appended
	nowTs := time.Now().Unix() * 1000
	for i := range interfaceClients {
		if cm, ok := interfaceClients[i].(map[string]any); ok {
			if _, ok2 := cm["created_at"]; !ok2 {
				cm["created_at"] = nowTs
			}
			cm["updated_at"] = nowTs
			interfaceClients[i] = cm
		}
	}
	existEmail, err := s.checkEmailsExistForClients(clients)
	if err != nil {
		return false, err
	}
	if existEmail != "" {
		return false, common.NewError("Duplicate email:", existEmail)
	}

	oldInbound, err := s.GetInbound(data.Id)
	if err != nil {
		return false, err
	}

	// Secure client ID
	for _, client := range clients {
		switch oldInbound.Protocol {
		case "trojan":
			if client.Password == "" {
				return false, common.NewError("empty client ID")
			}
		case "shadowsocks":
			if client.Email == "" {
				return false, common.NewError("empty client ID")
			}
		case "hysteria":
			if client.Auth == "" {
				return false, common.NewError("empty client ID")
			}
		default:
			if client.ID == "" {
				return false, common.NewError("empty client ID")
			}
		}
	}

	if oldInbound.NodeID != nil {
		node, remoteID, err := s.getInboundNode(oldInbound)
		if err != nil {
			return false, err
		}
		payload := *data
		payload.Id = remoteID
		payload.NodeID = nil
		payload.NodeInboundID = nil
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := (&NodeService{}).AddRemoteClient(ctx, node, &payload); err != nil {
			return false, err
		}
		_, err = (&NodeService{}).SyncNode(ctx, node.Id)
		return false, err
	}

	oldSettings, err := inboundSettingsObject(oldInbound.Settings, oldInbound.Id)
	if err != nil {
		return false, err
	}

	oldClients, err := inboundSettingsClients(oldSettings, oldInbound.Id)
	if err != nil {
		return false, err
	}
	oldClients = append(oldClients, interfaceClients...)

	oldSettings["clients"] = oldClients

	newSettings, err := json.MarshalIndent(oldSettings, "", "  ")
	if err != nil {
		return false, err
	}

	oldInbound.Settings = string(newSettings)

	db := database.GetDB()
	tx := db.Begin()

	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	needRestart := false
	s.xrayApi.Init(p.GetAPIPort())
	for _, client := range clients {
		if len(client.Email) > 0 {
			if err = s.AddClientStat(tx, data.Id, &client); err != nil {
				return false, err
			}
			if client.Enable {
				cipher := ""
				if oldInbound.Protocol == "shadowsocks" {
					cipher = oldSettings["method"].(string)
				}
				err1 := s.xrayApi.AddUser(string(oldInbound.Protocol), oldInbound.Tag, map[string]any{
					"email":    client.Email,
					"id":       client.ID,
					"auth":     client.Auth,
					"security": client.Security,
					"flow":     client.Flow,
					"password": client.Password,
					"cipher":   cipher,
				})
				if err1 == nil {
					logger.Debug("Client added by api:", client.Email)
				} else {
					logger.Debug("Error in adding client by api:", err1)
					needRestart = true
				}
			}
		} else {
			needRestart = true
		}
	}
	s.xrayApi.Close()

	err = tx.Save(oldInbound).Error
	return needRestart, err
}

func (s *InboundService) AddMultiInboundClient(data *model.Inbound, selectedInboundIDs []int) (bool, error) {
	defer ClientLimitGateRefresh()
	clients, err := s.GetClients(data)
	if err != nil {
		return false, err
	}
	if len(clients) == 0 {
		return false, common.NewError("no client payload provided")
	}
	if err := validateClientIPLimits(clients); err != nil {
		return false, err
	}

	baseClient := clients[len(clients)-1]
	if strings.TrimSpace(baseClient.SubID) == "" {
		baseClient.SubID = random.Seq(16)
	}

	targetInboundIDs := normalizeMultiInboundIDs(data.Id, selectedInboundIDs)
	if len(targetInboundIDs) == 0 {
		return false, common.NewError("no inbound selected for multi link")
	}

	allEmails, err := s.getAllEmails()
	if err != nil {
		return false, err
	}
	usedEmails := make(map[string]struct{}, len(allEmails))
	for _, email := range allEmails {
		usedEmails[strings.ToLower(strings.TrimSpace(email))] = struct{}{}
	}

	needRestart := false
	for idx, inboundID := range targetInboundIDs {
		targetInbound, err := s.GetInbound(inboundID)
		if err != nil {
			return needRestart, err
		}
		client := cloneClientForProtocol(targetInbound.Protocol, baseClient)
		if idx == 0 {
			firstEmail := strings.ToLower(strings.TrimSpace(client.Email))
			if firstEmail == "" {
				firstEmail = s.allocateMultiLinkEmail(baseClient.Email, inboundID, usedEmails)
			} else if _, exists := usedEmails[firstEmail]; exists {
				firstEmail = s.allocateMultiLinkEmail(baseClient.Email, inboundID, usedEmails)
			} else {
				usedEmails[firstEmail] = struct{}{}
			}
			client.Email = firstEmail
		} else {
			client.Email = s.allocateMultiLinkEmail(baseClient.Email, inboundID, usedEmails)
		}

		settingsBody, err := json.Marshal(map[string]any{
			"clients": []model.Client{client},
		})
		if err != nil {
			return needRestart, err
		}

		payload := &model.Inbound{
			Id:       inboundID,
			Settings: string(settingsBody),
		}
		restartOne, err := s.AddInboundClient(payload)
		if err != nil {
			return needRestart, err
		}
		needRestart = needRestart || restartOne
	}

	return needRestart, nil
}

func (s *InboundService) DelInboundClient(inboundId int, clientId string) (bool, error) {
	defer ClientLimitGateRefresh()
	oldInbound, err := s.GetInbound(inboundId)
	if err != nil {
		logger.Error("Load Old Data Error")
		return false, err
	}
	if oldInbound.NodeID != nil {
		node, remoteID, err := s.getInboundNode(oldInbound)
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := (&NodeService{}).DeleteRemoteClient(ctx, node, remoteID, clientId); err != nil {
			return false, err
		}
		_, err = (&NodeService{}).SyncNode(ctx, node.Id)
		return false, err
	}
	settings, err := inboundSettingsObject(oldInbound.Settings, oldInbound.Id)
	if err != nil {
		return false, err
	}

	email := ""
	client_key := "id"
	switch oldInbound.Protocol {
	case "trojan":
		client_key = "password"
	case "shadowsocks":
		client_key = "email"
	case "hysteria":
		client_key = "auth"
	}

	interfaceClients, err := inboundSettingsClients(settings, oldInbound.Id)
	if err != nil {
		return false, err
	}
	var newClients []any
	needApiDel := false
	for _, client := range interfaceClients {
		c := client.(map[string]any) // validated by inboundSettingsClients
		c_id, _ := c[client_key].(string)
		if c_id == clientId {
			email, _ = c["email"].(string)
			needApiDel, _ = c["enable"].(bool)
		} else {
			newClients = append(newClients, client)
		}
	}

	settings["clients"] = newClients
	newSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}

	oldInbound.Settings = string(newSettings)

	db := database.GetDB()

	err = s.DelClientIPs(db, email)
	if err != nil {
		logger.Error("Error in delete client IPs")
		return false, err
	}
	needRestart := false

	if len(email) > 0 {
		notDepleted := true
		err = db.Model(xray.ClientTraffic{}).Select("enable").Where("email = ?", email).First(&notDepleted).Error
		if err != nil {
			logger.Error("Get stats error")
			return false, err
		}
		err = s.DelClientStat(db, email)
		if err != nil {
			logger.Error("Delete stats Data Error")
			return false, err
		}
		if needApiDel && notDepleted {
			// If the Xray process is not running, skip the runtime API call;
			// the database state stays authoritative and a rebuild on the next
			// start reflects the deletion.
			if p == nil {
				needRestart = true
			} else {
				s.xrayApi.Init(p.GetAPIPort())
				err1 := s.xrayApi.RemoveUser(oldInbound.Tag, email)
				if err1 == nil {
					logger.Debug("Client deleted by api:", email)
					needRestart = false
				} else {
					if strings.Contains(err1.Error(), fmt.Sprintf("User %s not found.", email)) {
						logger.Debug("User is already deleted. Nothing to do more...")
					} else {
						logger.Debug("Error in deleting client by api:", err1)
						needRestart = true
					}
				}
				s.xrayApi.Close()
			}
		}
	}
	return needRestart, db.Save(oldInbound).Error
}

// inboundClientQuotaResolved reports whether a client being saved as disabled
// was automatically disabled by quota depletion or expiry and whether the new
// limits resolve that condition. It distinguishes an automatic disable (the
// old limits were already exhausted at the current usage) from a manual one,
// so manually disabled clients are never re-enabled implicitly.
//
// This is the legacy fallback for clients disabled before the disable-reason
// marker existed (empty DisableReason); marker-aware callers use
// clientLimitsResolved instead.
func (s *InboundService) inboundClientQuotaResolved(oldClient, newClient *model.Client, oldEmail string) (bool, error) {
	if oldClient == nil || newClient == nil || newClient.Enable {
		return false, nil
	}
	usage := &xray.ClientTraffic{}
	err := database.GetDB().Model(xray.ClientTraffic{}).
		Where("LOWER(email) = LOWER(?)", oldEmail).First(usage).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, err
		}
		// No traffic row yet: an expired client can still be re-enabled by an
		// expiry extension.
		usage = nil
	}
	now := time.Now().Unix() * 1000
	wasExpired := oldClient.ExpiryTime > 0 && oldClient.ExpiryTime <= now
	wasDepleted := false
	oldQuota := model.InitialClientTrafficQuota(*oldClient)
	if usage != nil && usage.Total > 0 {
		oldQuota = usage.Total
	}
	if usage != nil && oldQuota > 0 && usage.Up+usage.Down >= oldQuota {
		wasDepleted = true
	}
	if !wasExpired && !wasDepleted {
		// The old limits were not exhausted: the disable was manual.
		return false, nil
	}
	isExpired := newClient.ExpiryTime > 0 && newClient.ExpiryTime <= now
	isDepleted := false
	newQuota := model.UpdatedClientTrafficQuota(*oldClient, *newClient, oldQuota)
	if usage != nil && newQuota > 0 && usage.Up+usage.Down >= newQuota {
		isDepleted = true
	}
	return !isExpired && !isDepleted, nil
}

// clientLimitsResolved reports whether the limits saved in newClient still
// leave the client expired or quota-depleted at its current usage. oldEmail is
// used for the usage lookup because it may differ from newClient.Email when
// the client is being renamed.
func (s *InboundService) clientLimitsResolved(oldClient, newClient *model.Client, oldEmail string) (bool, error) {
	if oldClient == nil || newClient == nil {
		return false, nil
	}
	usage := &xray.ClientTraffic{}
	err := database.GetDB().Model(xray.ClientTraffic{}).
		Where("LOWER(email) = LOWER(?)", oldEmail).First(usage).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, err
		}
		// No traffic row yet: an expired client can still be re-enabled by an
		// expiry extension.
		usage = nil
	}
	now := time.Now().Unix() * 1000
	if newClient.ExpiryTime > 0 && newClient.ExpiryTime <= now {
		return false, nil
	}
	currentQuota := model.InitialClientTrafficQuota(*oldClient)
	if usage != nil && usage.Total > 0 {
		currentQuota = usage.Total
	}
	newQuota := model.UpdatedClientTrafficQuota(*oldClient, *newClient, currentQuota)
	if usage != nil && newQuota > 0 && usage.Up+usage.Down >= newQuota {
		return false, nil
	}
	return true, nil
}

// cohortStillDepletedForClient reports whether the traffic job would still
// consider this client's cohort (same email or subscription id) depleted or
// expired after the edited limits are applied. The job is the authoritative
// depletion check; consulting it here prevents an edit from re-enabling a
// client that the very next traffic tick would disable again.
func (s *InboundService) cohortStillDepletedForClient(oldEmail string, newClient *model.Client) (bool, error) {
	if newClient == nil {
		return false, nil
	}
	db := database.GetDB()
	now := time.Now().Unix() * 1000
	cohorts, err := s.depletedQuotaCohorts(db, now)
	if err != nil {
		return false, err
	}
	email := strings.ToLower(strings.TrimSpace(oldEmail))
	key := email
	if subID := strings.TrimSpace(newClient.SubID); subID != "" {
		key = "sub:" + subID
	}
	cohort, ok := cohorts[key]
	if !ok {
		return false, nil
	}
	// Re-evaluate the cohort with this client's edited limits applied (the
	// client counts as enabled because we are evaluating a re-enable).
	limit := int64(0)
	expired := false
	for _, target := range cohort.Targets {
		totalGB, expiryTime, clientEnable := target.TotalGB, target.ExpiryTime, target.ClientEnable
		if target.Email == email {
			oldForQuota := model.Client{TotalGB: target.NominalTotalGB, QuotaFactor: target.QuotaFactor}
			totalGB = model.UpdatedClientTrafficQuota(oldForQuota, *newClient, target.TotalGB)
			expiryTime, clientEnable = newClient.ExpiryTime, true
		}
		if clientEnable && expiryTime > 0 && expiryTime <= now {
			expired = true
		}
		if totalGB > 0 && totalGB > limit {
			limit = totalGB
		}
	}
	if expired {
		return true, nil
	}
	return limit > 0 && cohort.Used >= limit, nil
}

// classifyDisableReason reconciles the stored disable reason with the one
// carried by the save payload: a manual marker on either side wins (a manual
// disable must survive edits), otherwise the payload's reason (frontend
// round-trip), otherwise the stored reason. An empty result means the disable
// predates the marker (legacy data).
func classifyDisableReason(stored, payload string) string {
	if payload == DisableReasonManual || stored == DisableReasonManual {
		return DisableReasonManual
	}
	if payload != "" {
		return payload
	}
	return stored
}

// shouldReEnableClientOnEdit decides whether an edit that saves the client as
// disabled may re-enable it. It never re-enables a manual disable. An
// automatic disable (quota/expiry marker) is resolved only when the new limits
// no longer deplete the client AND the traffic job's cohort check agrees, so
// an edit cannot enable a client that the next traffic tick would disable
// again. A reseller-lifecycle disable is only ever reversed by the lifecycle
// restore. Legacy disables without a marker fall back to the old-limits
// heuristic that detects an automatic disable.
func (s *InboundService) shouldReEnableClientOnEdit(oldClient, newClient *model.Client, oldEmail string) (bool, error) {
	if oldClient == nil || newClient == nil || newClient.Enable {
		return false, nil
	}
	if oldClient.Enable {
		// The client is currently enabled: saving it disabled is a manual act.
		return false, nil
	}
	switch classifyDisableReason(oldClient.DisableReason, newClient.DisableReason) {
	case DisableReasonManual:
		return false, nil
	case DisableReasonReseller:
		// Only the reseller lifecycle restore re-enables these clients.
		return false, nil
	case DisableReasonQuota, DisableReasonExpiry:
		resolved, err := s.clientLimitsResolved(oldClient, newClient, oldEmail)
		if err != nil || !resolved {
			return false, err
		}
		depleted, err := s.cohortStillDepletedForClient(oldEmail, newClient)
		if err != nil {
			return false, err
		}
		return !depleted, nil
	default:
		// Legacy disable without a recorded reason: the old-limits heuristic
		// detects whether the disable was automatic (preserves the re-enable
		// for clients auto-disabled before the marker existed).
		resolved, err := s.inboundClientQuotaResolved(oldClient, newClient, oldEmail)
		if err != nil || !resolved {
			return false, err
		}
		depleted, err := s.cohortStillDepletedForClient(oldEmail, newClient)
		if err != nil {
			return false, err
		}
		return !depleted, nil
	}
}

// disableReasonForSavedClient returns the disable reason to persist for a
// client saved as disabled. Manual and reseller-lifecycle reasons are kept
// as-is; a quota/expiry reason is refreshed from the new limits so the marker
// tracks the condition that currently holds the client disabled. A legacy
// disable keeps whatever the payload carried (empty for old data).
func disableReasonForSavedClient(oldClient, newClient *model.Client) string {
	if oldClient == nil || newClient == nil {
		return ""
	}
	if oldClient.Enable && !newClient.Enable {
		// Saving a disabled client over an enabled one is a manual act, unless
		// the payload records its own reason (e.g. the reseller lifecycle).
		if newClient.DisableReason != "" {
			return newClient.DisableReason
		}
		return DisableReasonManual
	}
	reason := classifyDisableReason(oldClient.DisableReason, newClient.DisableReason)
	switch reason {
	case DisableReasonManual, DisableReasonReseller:
		return reason
	case DisableReasonQuota, DisableReasonExpiry:
		now := time.Now().Unix() * 1000
		if newClient.ExpiryTime > 0 && newClient.ExpiryTime <= now {
			return DisableReasonExpiry
		}
		return DisableReasonQuota
	default:
		return reason
	}
}

func (s *InboundService) UpdateInboundClient(data *model.Inbound, clientId string) (bool, error) {
	defer ClientLimitGateRefresh()
	// TODO: check if TrafficReset field is updating
	clients, err := s.GetClients(data)
	if err != nil {
		return false, err
	}
	if len(clients) == 0 {
		return false, common.NewError("no client payload provided")
	}
	if err := validateClientIPLimits(clients); err != nil {
		return false, err
	}

	var settings map[string]any
	err = json.Unmarshal([]byte(data.Settings), &settings)
	if err != nil {
		return false, err
	}

	interfaceClients, ok := settings["clients"].([]any)
	if !ok || len(interfaceClients) == 0 {
		return false, common.NewError("invalid clients list in request settings")
	}

	oldInbound, err := s.GetInbound(data.Id)
	if err != nil {
		return false, err
	}
	if oldInbound.NodeID != nil {
		node, _, err := s.getInboundNode(oldInbound)
		if err != nil {
			return false, err
		}
		payload := *data
		payload.NodeID = nil
		payload.NodeInboundID = nil
		if oldInbound.NodeInboundID != nil {
			payload.Id = *oldInbound.NodeInboundID
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := (&NodeService{}).UpdateRemoteClient(ctx, node, clientId, &payload); err != nil {
			return false, err
		}
		_, err = (&NodeService{}).SyncNode(ctx, node.Id)
		return false, err
	}

	oldClients, err := s.GetClients(oldInbound)
	if err != nil {
		return false, err
	}

	oldEmail := ""
	newClientId := ""
	clientIndex := -1
	for index, oldClient := range oldClients {
		oldClientId := ""
		switch oldInbound.Protocol {
		case "trojan":
			oldClientId = oldClient.Password
			newClientId = clients[0].Password
		case "shadowsocks":
			oldClientId = oldClient.Email
			newClientId = clients[0].Email
		case "hysteria":
			oldClientId = oldClient.Auth
			newClientId = clients[0].Auth
		default:
			oldClientId = oldClient.ID
			newClientId = clients[0].ID
		}
		if clientId == oldClientId {
			oldEmail = oldClient.Email
			clientIndex = index
			break
		}
	}

	// Validate new client ID
	if newClientId == "" || clientIndex == -1 {
		return false, common.NewError("empty client ID")
	}

	if len(clients[0].Email) > 0 && clients[0].Email != oldEmail {
		existEmail, err := s.checkEmailsExistForClients(clients)
		if err != nil {
			return false, err
		}
		if existEmail != "" {
			return false, common.NewError("Duplicate email:", existEmail)
		}
	}
	previousClient := oldClients[clientIndex]
	if clients[0].QuotaFactor == nil && previousClient.QuotaFactor != nil {
		factor := *previousClient.QuotaFactor
		clients[0].QuotaFactor = &factor
	}

	// A client saved as disabled keeps its disabled state unless it was
	// automatically disabled (recorded disable reason) and the new limits
	// resolve that condition: increasing the volume or extending the expiry
	// re-enables an auto-disabled client. A manual disable is never reversed
	// implicitly. See shouldReEnableClientOnEdit for the full rules.
	if !clients[0].Enable {
		reEnable, rerr := s.shouldReEnableClientOnEdit(&oldClients[clientIndex], &clients[0], oldEmail)
		if rerr != nil {
			return false, rerr
		}
		if reEnable {
			clients[0].Enable = true
			if newMap, ok := interfaceClients[0].(map[string]any); ok {
				newMap["enable"] = true
			}
		}
	}
	if previousClient.OwnerUserID > 0 {
		clients[0].OwnerUserID = previousClient.OwnerUserID
	}
	clients[0].Comment = mergeResellerOwnershipTags(
		clients[0].Comment,
		previousClient.Comment,
		previousClient.OwnerUserID,
		clients[0].OwnerUserID,
	)
	if !clients[0].Enable && clients[0].DisableReason == "" && previousClient.DisableReason != "" {
		clients[0].DisableReason = previousClient.DisableReason
	}

	var oldSettings map[string]any
	err = json.Unmarshal([]byte(oldInbound.Settings), &oldSettings)
	if err != nil {
		return false, err
	}
	settingsClients, ok := oldSettings["clients"].([]any)
	if !ok || clientIndex >= len(settingsClients) {
		return false, common.NewError("invalid clients list in stored inbound settings")
	}
	// Preserve created_at and set updated_at for the replacing client
	var preservedCreated any
	if clientIndex >= 0 && clientIndex < len(settingsClients) {
		if oldMap, ok := settingsClients[clientIndex].(map[string]any); ok {
			if v, ok2 := oldMap["created_at"]; ok2 {
				preservedCreated = v
			}
		}
	}
	if len(interfaceClients) > 0 {
		if newMap, ok := interfaceClients[0].(map[string]any); ok {
			newMap["ownerUserId"] = clients[0].OwnerUserID
			newMap["comment"] = clients[0].Comment
			if clients[0].QuotaFactor != nil {
				newMap["quotaFactor"] = *clients[0].QuotaFactor
			}
			if clients[0].DisableReason != "" {
				newMap["disableReason"] = clients[0].DisableReason
			}
			if preservedCreated == nil {
				preservedCreated = time.Now().Unix() * 1000
			}
			newMap["created_at"] = preservedCreated
			newMap["updated_at"] = time.Now().Unix() * 1000
			if clients[0].Enable {
				// An enabled client carries no disable reason.
				delete(newMap, "disableReason")
			} else if reason := disableReasonForSavedClient(&oldClients[clientIndex], &clients[0]); reason != "" {
				newMap["disableReason"] = reason
			}
			interfaceClients[0] = newMap
		}
	}
	settingsClients[clientIndex] = interfaceClients[0]
	oldSettings["clients"] = settingsClients

	newSettings, err := json.MarshalIndent(oldSettings, "", "  ")
	if err != nil {
		return false, err
	}

	oldInbound.Settings = string(newSettings)
	db := database.GetDB()
	tx := db.Begin()

	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	if len(clients[0].Email) > 0 {
		if len(oldEmail) > 0 {
			err = s.UpdateClientStat(tx, oldEmail, &previousClient, &clients[0])
			if err != nil {
				return false, err
			}
			err = s.UpdateClientIPs(tx, oldEmail, clients[0].Email)
			if err != nil {
				return false, err
			}
			if previousClient.OwnerUserID > 0 && !strings.EqualFold(oldEmail, clients[0].Email) {
				if err = migrateResellerClientStateKey(tx, previousClient.OwnerUserID, data.Id, oldEmail, clients[0].Email); err != nil {
					return false, err
				}
			}
		} else {
			s.AddClientStat(tx, data.Id, &clients[0])
		}
	} else {
		err = s.DelClientStat(tx, oldEmail)
		if err != nil {
			return false, err
		}
		err = s.DelClientIPs(tx, oldEmail)
		if err != nil {
			return false, err
		}
	}
	needRestart := false
	if len(oldEmail) > 0 {
		// If the Xray process is not running, skip the runtime API calls;
		// the database state stays authoritative and a restart is requested.
		if p != nil {
			s.xrayApi.Init(p.GetAPIPort())
		}
		if p == nil {
			needRestart = true
		}
		if oldClients[clientIndex].Enable {
			err1 := s.xrayApi.RemoveUser(oldInbound.Tag, oldEmail)
			if err1 == nil {
				logger.Debug("Old client deleted by api:", oldEmail)
			} else {
				if strings.Contains(err1.Error(), fmt.Sprintf("User %s not found.", oldEmail)) {
					logger.Debug("User is already deleted. Nothing to do more...")
				} else {
					logger.Debug("Error in deleting client by api:", err1)
					needRestart = true
				}
			}
		}
		if clients[0].Enable {
			cipher := ""
			if oldInbound.Protocol == "shadowsocks" {
				cipher = oldSettings["method"].(string)
			}
			err1 := s.xrayApi.AddUser(string(oldInbound.Protocol), oldInbound.Tag, map[string]any{
				"email":    clients[0].Email,
				"id":       clients[0].ID,
				"security": clients[0].Security,
				"flow":     clients[0].Flow,
				"auth":     clients[0].Auth,
				"password": clients[0].Password,
				"cipher":   cipher,
			})
			if err1 == nil {
				logger.Debug("Client edited by api:", clients[0].Email)
			} else {
				logger.Debug("Error in adding client by api:", err1)
				needRestart = true
			}
		}
		s.xrayApi.Close()
	} else {
		logger.Debug("Client old email not found")
		needRestart = true
	}
	err = tx.Save(oldInbound).Error
	if err == nil {
		// Limits, grace period, enable flag or email may have changed: make
		// the IP limit engine drop its cached policy for this client.
		clientLimitEngineNotifyClientUpdated(oldEmail, clients[0].Email)
	}
	return needRestart, err
}

func (s *InboundService) AddTraffic(inboundTraffics []*xray.Traffic, clientTraffics []*xray.ClientTraffic) (error, bool) {
	startupReconcile := p == nil
	if startupReconcile {
		logger.Info("Xray startup: beginning traffic/client reconciliation transaction")
	}
	defer func() {
		if startupReconcile {
			logger.Info("Xray startup: refreshing IP-limit policy after traffic reconciliation")
		}
		ClientLimitGateRefresh()
		if startupReconcile {
			logger.Info("Xray startup: IP-limit policy refresh after traffic reconciliation finished")
		}
	}()
	var err error
	db := database.GetDB()
	tx := db.Begin()
	if startupReconcile {
		logger.Info("Xray startup: traffic reconciliation transaction opened")
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	err = s.addInboundTraffic(tx, inboundTraffics)
	if err != nil {
		return err, false
	}
	err = s.addClientTraffic(tx, clientTraffics)
	if err != nil {
		return err, false
	}

	if startupReconcile {
		logger.Info("Xray startup: checking auto-renew clients")
	}
	needRestart0, count, err := s.autoRenewClients(tx)
	if startupReconcile {
		logger.Info("Xray startup: auto-renew reconciliation query finished")
	}
	if err != nil {
		logger.Warning("Error in renew clients:", err)
	} else if count > 0 {
		logger.Debugf("%v clients renewed", count)
	}

	if startupReconcile {
		logger.Info("Xray startup: checking client quota/expiry limits")
	}
	needRestart1, count, err := s.disableInvalidClients(tx)
	if startupReconcile {
		logger.Info("Xray startup: client quota/expiry reconciliation query finished")
	}
	if err != nil {
		logger.Warning("Error in disabling invalid clients:", err)
	} else if count > 0 {
		logger.Debugf("%v clients disabled", count)
	}

	if startupReconcile {
		logger.Info("Xray startup: checking inbound limits")
	}
	needRestart2, count, err := s.disableInvalidInbounds(tx)
	if startupReconcile {
		logger.Info("Xray startup: inbound reconciliation query finished")
	}
	if err != nil {
		logger.Warning("Error in disabling invalid inbounds:", err)
	} else if count > 0 {
		logger.Debugf("%v inbounds disabled", count)
	}
	return nil, (needRestart0 || needRestart1 || needRestart2)
}

func (s *InboundService) addInboundTraffic(tx *gorm.DB, traffics []*xray.Traffic) error {
	if len(traffics) == 0 {
		return nil
	}

	var err error

	for _, traffic := range traffics {
		if traffic.IsInbound {
			err = tx.Model(&model.Inbound{}).Where("tag = ?", traffic.Tag).
				Updates(map[string]any{
					"up":       gorm.Expr("up + ?", traffic.Up),
					"down":     gorm.Expr("down + ?", traffic.Down),
					"all_time": gorm.Expr("COALESCE(all_time, 0) + ?", traffic.Up+traffic.Down),
				}).Error
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *InboundService) addClientTraffic(tx *gorm.DB, traffics []*xray.ClientTraffic) (err error) {
	if len(traffics) == 0 {
		// Empty onlineUsers
		if p != nil {
			p.SetOnlineClients(make([]string, 0))
		}
		return nil
	}

	onlineClients := make([]string, 0)
	trafficByEmail := make(map[string]*xray.ClientTraffic, len(traffics))
	for _, traffic := range traffics {
		if traffic == nil {
			continue
		}
		if _, exists := trafficByEmail[traffic.Email]; !exists {
			trafficByEmail[traffic.Email] = traffic
		}
	}

	emails := make([]string, 0, len(traffics))
	for _, traffic := range traffics {
		if traffic == nil {
			continue
		}
		emails = append(emails, traffic.Email)
	}
	dbClientTraffics := make([]*xray.ClientTraffic, 0, len(traffics))
	err = tx.Model(xray.ClientTraffic{}).Where("email IN (?)", emails).Find(&dbClientTraffics).Error
	if err != nil {
		return err
	}

	// Avoid empty slice error
	if len(dbClientTraffics) == 0 {
		return nil
	}

	dbClientTraffics, err = s.adjustTraffics(tx, dbClientTraffics)
	if err != nil {
		return err
	}

	for _, dbTraffic := range dbClientTraffics {
		traffic := trafficByEmail[dbTraffic.Email]
		if traffic == nil {
			continue
		}
		dbTraffic.Up += traffic.Up
		dbTraffic.Down += traffic.Down
		dbTraffic.AllTime += traffic.Up + traffic.Down

		// Add user in onlineUsers array on traffic.
		if traffic.Up+traffic.Down > 0 {
			onlineClients = append(onlineClients, traffic.Email)
			dbTraffic.LastOnline = time.Now().UnixMilli()
		}
	}

	// Set onlineUsers
	if p != nil {
		p.SetOnlineClients(onlineClients)
	}

	err = tx.Save(dbClientTraffics).Error
	if err != nil {
		logger.Warning("AddClientTraffic update data ", err)
	}

	return nil
}

func (s *InboundService) adjustTraffics(tx *gorm.DB, dbClientTraffics []*xray.ClientTraffic) ([]*xray.ClientTraffic, error) {
	updated := make(map[int]*model.Inbound)
	for traffic_index := range dbClientTraffics {
		if dbClientTraffics[traffic_index].ExpiryTime >= 0 {
			continue
		}

		inbound, err := s.GetInbound(dbClientTraffics[traffic_index].InboundId)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			inbound, err = s.getInboundByEmail(tx, dbClientTraffics[traffic_index].Email)
		}
		if err != nil || inbound == nil {
			continue
		}

		settings, err := inboundSettingsObject(inbound.Settings, inbound.Id)
		if err != nil {
			return nil, err
		}
		clients, err := inboundSettingsClients(settings, inbound.Id)
		if err != nil {
			return nil, err
		}

		nowMs := time.Now().Unix() * 1000
		changed := false
		newClients := make([]any, 0, len(clients))
		for client_index := range clients {
			c := clients[client_index].(map[string]any)
			email, _ := c["email"].(string)
			if dbClientTraffics[traffic_index].ExpiryTime < 0 && email == dbClientTraffics[traffic_index].Email {
				oldExpiryTime, ok := c["expiryTime"].(float64)
				if !ok {
					return nil, fmt.Errorf("client %q in inbound %d has invalid expiryTime", email, inbound.Id)
				}
				newExpiryTime := nowMs - int64(oldExpiryTime)
				c["expiryTime"] = newExpiryTime
				c["updated_at"] = nowMs
				dbClientTraffics[traffic_index].ExpiryTime = newExpiryTime
				changed = true
			}
			if _, ok := c["created_at"]; !ok {
				c["created_at"] = nowMs
				changed = true
			}
			c["updated_at"] = nowMs
			newClients = append(newClients, any(c))
		}
		if !changed {
			continue
		}
		settings["clients"] = newClients
		modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return nil, err
		}
		inbound.Settings = string(modifiedSettings)
		updated[inbound.Id] = inbound
	}

	if len(updated) > 0 {
		inbounds := make([]*model.Inbound, 0, len(updated))
		for _, inbound := range updated {
			inbounds = append(inbounds, inbound)
		}
		err := tx.Save(inbounds).Error
		if err != nil {
			logger.Warning("AddClientTraffic update inbounds ", err)
			logger.Error(inbounds)
		}
	}

	return dbClientTraffics, nil
}

func (s *InboundService) autoRenewClients(tx *gorm.DB) (bool, int64, error) {
	// check for time expired
	var traffics []*xray.ClientTraffic
	now := time.Now().Unix() * 1000
	var err, err1 error

	err = tx.Model(xray.ClientTraffic{}).Where("reset > 0 and expiry_time > 0 and expiry_time <= ?", now).Find(&traffics).Error
	if err != nil {
		return false, 0, err
	}
	// return if there is no client to renew
	if len(traffics) == 0 {
		return false, 0, nil
	}

	var inbound_ids []int
	var inbounds []*model.Inbound
	needRestart := false
	var clientsToAdd []struct {
		protocol string
		tag      string
		client   map[string]any
	}

	for _, traffic := range traffics {
		inbound_ids = append(inbound_ids, traffic.InboundId)
	}
	trafficIndexByEmail := make(map[string]int, len(traffics))
	for trafficIndex, traffic := range traffics {
		if traffic != nil {
			trafficIndexByEmail[traffic.Email] = trafficIndex
		}
	}
	err = tx.Model(model.Inbound{}).Where("id IN ?", inbound_ids).Find(&inbounds).Error
	if err != nil {
		return false, 0, err
	}
	for inbound_index := range inbounds {
		settings, err := inboundSettingsObject(inbounds[inbound_index].Settings, inbounds[inbound_index].Id)
		if err != nil {
			return false, 0, err
		}
		clients, err := inboundSettingsClients(settings, inbounds[inbound_index].Id)
		if err != nil {
			return false, 0, err
		}
		for client_index := range clients {
			c := clients[client_index].(map[string]any)
			email, _ := c["email"].(string)
			if traffic_index, ok := trafficIndexByEmail[email]; ok {
				traffic := traffics[traffic_index]
				newExpiryTime := traffic.ExpiryTime
				for newExpiryTime < now {
					newExpiryTime += (int64(traffic.Reset) * 86400000)
				}
				c["expiryTime"] = newExpiryTime
				traffics[traffic_index].ExpiryTime = newExpiryTime
				traffics[traffic_index].Down = 0
				traffics[traffic_index].Up = 0
				var renewedClient model.Client
				if clientJSON, marshalErr := json.Marshal(c); marshalErr == nil {
					if unmarshalErr := json.Unmarshal(clientJSON, &renewedClient); unmarshalErr == nil {
						traffics[traffic_index].Total = model.InitialClientTrafficQuota(renewedClient)
					}
				}
				if !traffic.Enable {
					// Renew the expiry and usage for every periodic client,
					// but only re-enable clients that were disabled
					// automatically: a manual (or reseller lifecycle)
					// disable must never be reversed by the traffic job.
					if reason, _ := c["disableReason"].(string); reason != DisableReasonManual && reason != DisableReasonReseller {
						traffics[traffic_index].Enable = true
						c["enable"] = true
						delete(c, "disableReason")
						clientsToAdd = append(clientsToAdd,
							struct {
								protocol string
								tag      string
								client   map[string]any
							}{
								protocol: string(inbounds[inbound_index].Protocol),
								tag:      inbounds[inbound_index].Tag,
								client:   c,
							})
					}
				}
				clients[client_index] = any(c)
			}
		}
		settings["clients"] = clients
		newSettings, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return false, 0, err
		}
		inbounds[inbound_index].Settings = string(newSettings)
	}
	err = tx.Save(inbounds).Error
	if err != nil {
		return false, 0, err
	}
	err = tx.Save(traffics).Error
	if err != nil {
		return false, 0, err
	}
	if p != nil {
		err1 = s.xrayApi.Init(p.GetAPIPort())
		if err1 != nil {
			return true, int64(len(traffics)), nil
		}
		for _, clientToAdd := range clientsToAdd {
			err1 = s.xrayApi.AddUser(clientToAdd.protocol, clientToAdd.tag, clientToAdd.client)
			if err1 != nil {
				needRestart = true
			}
		}
		s.xrayApi.Close()
	}
	return needRestart, int64(len(traffics)), nil
}

func (s *InboundService) disableInvalidInbounds(tx *gorm.DB) (bool, int64, error) {
	now := time.Now().Unix() * 1000
	needRestart := false

	if p != nil {
		var tags []string
		err := tx.Table("inbounds").
			Select("inbounds.tag").
			Where("((total > 0 and up + down >= total) or (expiry_time > 0 and expiry_time <= ?)) and enable = ?", now, true).
			Scan(&tags).Error
		if err != nil {
			return false, 0, err
		}
		s.xrayApi.Init(p.GetAPIPort())
		for _, tag := range tags {
			err1 := s.xrayApi.DelInbound(tag)
			if err1 == nil {
				logger.Debug("Inbound disabled by api:", tag)
			} else {
				logger.Debug("Error in disabling inbound by api:", err1)
				needRestart = true
			}
		}
		s.xrayApi.Close()
	}

	result := tx.Model(model.Inbound{}).
		Where("((total > 0 and up + down >= total) or (expiry_time > 0 and expiry_time <= ?)) and enable = ?", now, true).
		Update("enable", false)
	err := result.Error
	count := result.RowsAffected
	return needRestart, count, err
}

func (s *InboundService) disableInvalidClients(tx *gorm.DB) (bool, int64, error) {
	now := time.Now().Unix() * 1000
	needRestart := false
	cohorts, err := s.depletedQuotaCohorts(tx, now)
	if err != nil {
		return false, 0, err
	}

	depletedEmails := make(map[string]struct{})
	inboundEmailReasons := make(map[int]map[string]string)
	localTargets := make([]quotaTarget, 0)
	for _, cohort := range cohorts {
		if !cohort.Depleted {
			continue
		}
		reason := DisableReasonQuota
		if cohort.Expired {
			reason = DisableReasonExpiry
		}
		for email := range cohort.Emails {
			depletedEmails[email] = struct{}{}
		}
		for _, target := range cohort.Targets {
			if inboundEmailReasons[target.InboundID] == nil {
				inboundEmailReasons[target.InboundID] = map[string]string{}
			}
			inboundEmailReasons[target.InboundID][target.Email] = reason
			if target.NodeID == nil {
				localTargets = append(localTargets, target)
			}
		}
	}
	if len(depletedEmails) == 0 {
		return false, 0, nil
	}

	if p != nil && len(localTargets) > 0 {
		s.xrayApi.Init(p.GetAPIPort())
		for _, target := range localTargets {
			err1 := s.xrayApi.RemoveUser(target.Tag, target.Email)
			if err1 == nil {
				logger.Debug("Client disabled by api:", target.Email)
				continue
			}
			if strings.Contains(err1.Error(), fmt.Sprintf("User %s not found.", target.Email)) {
				logger.Debug("User is already disabled. Nothing to do more...")
				continue
			}
			logger.Debug("Error in disabling client by api:", err1)
			needRestart = true
		}
		s.xrayApi.Close()
	}

	var inbounds []*model.Inbound
	if err := tx.Model(model.Inbound{}).Find(&inbounds).Error; err != nil {
		return false, 0, err
	}
	for _, inbound := range inbounds {
		if emailReasons := inboundEmailReasons[inbound.Id]; len(emailReasons) > 0 {
			if err := s.setClientsDisabledInInbound(tx, inbound, emailReasons); err != nil {
				return false, 0, err
			}
		}
	}

	emails := make([]string, 0, len(depletedEmails))
	for email := range depletedEmails {
		emails = append(emails, email)
	}
	result := tx.Model(xray.ClientTraffic{}).Where("LOWER(email) IN ? AND enable = ?", emails, true).Update("enable", false)
	if result.Error != nil {
		return needRestart, 0, result.Error
	}
	if result.RowsAffected > 0 && len(localTargets) > 0 {
		// A restart guarantees lingering sessions are dropped after precise quota exhaustion.
		needRestart = true
	}
	return needRestart, result.RowsAffected, nil
}

func (s *InboundService) GetInboundTags() (string, error) {
	db := database.GetDB()
	var inboundTags []string
	err := db.Model(model.Inbound{}).Select("tag").Find(&inboundTags).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return "", err
	}
	tags, _ := json.Marshal(inboundTags)
	return string(tags), nil
}

func (s *InboundService) MigrationRemoveOrphanedTraffics() {
	db := database.GetDB()
	emails, err := s.getAllEmails()
	if err != nil {
		logger.Warning("remove orphaned traffics skipped:", err)
		return
	}
	if len(emails) == 0 {
		db.Delete(&xray.ClientTraffic{})
		return
	}
	db.Where("email NOT IN ?", emails).Delete(&xray.ClientTraffic{})
}

func (s *InboundService) AddClientStat(tx *gorm.DB, inboundId int, client *model.Client) error {
	clientTraffic := xray.ClientTraffic{
		InboundId:  inboundId,
		Email:      client.Email,
		Total:      model.InitialClientTrafficQuota(*client),
		ExpiryTime: client.ExpiryTime,
		Enable:     client.Enable,
		Up:         0,
		Down:       0,
		Reset:      client.Reset,
	}
	err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "email"}},
		DoNothing: true,
	}).Create(&clientTraffic).Error
	if err == nil {
		// New client: drop any "no policy" negative cache entry so the IP
		// limit engine starts tracking it immediately.
		clientLimitEngineNotifyClientUpdated("", client.Email)
	}
	return err
}

func (s *InboundService) UpdateClientStat(tx *gorm.DB, email string, oldClient, client *model.Client) error {
	current := xray.ClientTraffic{}
	currentTotal := int64(0)
	if err := tx.Where("LOWER(email) = LOWER(?)", email).First(&current).Error; err == nil {
		currentTotal = current.Total
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	quotaTotal := model.InitialClientTrafficQuota(*client)
	if oldClient != nil {
		quotaTotal = model.UpdatedClientTrafficQuota(*oldClient, *client, currentTotal)
	}
	result := tx.Model(xray.ClientTraffic{}).
		Where("LOWER(email) = LOWER(?)", email).
		Updates(map[string]any{
			"enable":      client.Enable,
			"email":       client.Email,
			"total":       quotaTotal,
			"expiry_time": client.ExpiryTime,
			"reset":       client.Reset,
		})
	err := result.Error
	return err
}

func (s *InboundService) UpdateClientIPs(tx *gorm.DB, oldEmail string, newEmail string) error {
	return tx.Model(model.InboundClientIps{}).Where("LOWER(client_email) = LOWER(?)", oldEmail).Update("client_email", newEmail).Error
}

func (s *InboundService) DelClientStat(tx *gorm.DB, email string) error {
	return tx.Where("LOWER(email) = LOWER(?)", email).Delete(xray.ClientTraffic{}).Error
}

func (s *InboundService) DelClientIPs(tx *gorm.DB, email string) error {
	err := tx.Where("LOWER(client_email) = LOWER(?)", email).Delete(model.InboundClientIps{}).Error
	if err == nil {
		// Client (or its IP bindings) deleted: forget live engine state too.
		clientLimitEngineForgetClient(email)
	}
	return err
}

func (s *InboundService) getInboundIDsForEmail(tx *gorm.DB, email string) ([]int, error) {
	if email == "" {
		return nil, nil
	}
	var inbounds []*model.Inbound
	if err := tx.Model(model.Inbound{}).Find(&inbounds).Error; err != nil {
		return nil, err
	}
	needle := strings.ToLower(email)
	ids := make([]int, 0, 1)
	for _, inbound := range inbounds {
		clients, err := s.GetClients(inbound)
		if err != nil {
			continue
		}
		for _, client := range clients {
			if strings.ToLower(client.Email) == needle {
				ids = append(ids, inbound.Id)
				break
			}
		}
	}
	return ids, nil
}

func (s *InboundService) getInboundByEmail(tx *gorm.DB, email string) (*model.Inbound, error) {
	ids, err := s.getInboundIDsForEmail(tx, email)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return s.GetInbound(ids[0])
}

func (s *InboundService) GetClientInboundByTrafficID(trafficId int) (traffic *xray.ClientTraffic, inbound *model.Inbound, err error) {
	db := database.GetDB()
	var traffics []*xray.ClientTraffic
	err = db.Model(xray.ClientTraffic{}).Where("id = ?", trafficId).Find(&traffics).Error
	if err != nil {
		logger.Warningf("Error retrieving ClientTraffic with trafficId %d: %v", trafficId, err)
		return nil, nil, err
	}
	if len(traffics) > 0 {
		inbound, err = s.GetInbound(traffics[0].InboundId)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			inbound, err = s.getInboundByEmail(db, traffics[0].Email)
		}
		return traffics[0], inbound, err
	}
	return nil, nil, nil
}

func (s *InboundService) GetClientInboundByEmail(email string) (traffic *xray.ClientTraffic, inbound *model.Inbound, err error) {
	db := database.GetDB()
	var traffics []*xray.ClientTraffic
	err = db.Model(xray.ClientTraffic{}).Where("email = ?", email).Find(&traffics).Error
	if err != nil {
		logger.Warningf("Error retrieving ClientTraffic with email %s: %v", email, err)
		return nil, nil, err
	}
	if len(traffics) > 0 {
		inbound, err = s.GetInbound(traffics[0].InboundId)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			inbound, err = s.getInboundByEmail(db, email)
		}
		return traffics[0], inbound, err
	}
	return nil, nil, nil
}

func (s *InboundService) GetClientByEmail(clientEmail string) (*xray.ClientTraffic, *model.Client, error) {
	traffic, inbound, err := s.GetClientInboundByEmail(clientEmail)
	if err != nil {
		return nil, nil, err
	}
	if inbound == nil {
		return nil, nil, common.NewError("Inbound Not Found For Email:", clientEmail)
	}

	clients, err := s.GetClients(inbound)
	if err != nil {
		return nil, nil, err
	}

	for _, client := range clients {
		if client.Email == clientEmail {
			return traffic, &client, nil
		}
	}

	return nil, nil, common.NewError("Client Not Found In Inbound For Email:", clientEmail)
}

func (s *InboundService) SetClientTelegramUserID(trafficId int, tgId int64) (bool, error) {
	traffic, inbound, err := s.GetClientInboundByTrafficID(trafficId)
	if err != nil {
		return false, err
	}
	if inbound == nil {
		return false, common.NewError("Inbound Not Found For Traffic ID:", trafficId)
	}

	clientEmail := traffic.Email

	oldClients, err := s.GetClients(inbound)
	if err != nil {
		return false, err
	}

	clientId := ""

	for _, oldClient := range oldClients {
		if oldClient.Email == clientEmail {
			switch inbound.Protocol {
			case "trojan":
				clientId = oldClient.Password
			case "shadowsocks":
				clientId = oldClient.Email
			default:
				clientId = oldClient.ID
			}
			break
		}
	}

	if len(clientId) == 0 {
		return false, common.NewError("Client Not Found For Email:", clientEmail)
	}

	settings, err := inboundSettingsObject(inbound.Settings, inbound.Id)
	if err != nil {
		return false, err
	}
	clients, err := inboundSettingsClients(settings, inbound.Id)
	if err != nil {
		return false, err
	}
	var newClients []any
	for client_index := range clients {
		c := clients[client_index].(map[string]any)
		if c["email"] == clientEmail {
			c["tgId"] = tgId
			c["updated_at"] = time.Now().Unix() * 1000
			newClients = append(newClients, any(c))
		}
	}
	settings["clients"] = newClients
	modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	inbound.Settings = string(modifiedSettings)
	needRestart, err := s.UpdateInboundClient(inbound, clientId)
	return needRestart, err
}

func (s *InboundService) checkIsEnabledByEmail(clientEmail string) (bool, error) {
	_, inbound, err := s.GetClientInboundByEmail(clientEmail)
	if err != nil {
		return false, err
	}
	if inbound == nil {
		return false, common.NewError("Inbound Not Found For Email:", clientEmail)
	}

	clients, err := s.GetClients(inbound)
	if err != nil {
		return false, err
	}

	isEnable := false

	for _, client := range clients {
		if client.Email == clientEmail {
			isEnable = client.Enable
			break
		}
	}

	return isEnable, err
}

func (s *InboundService) ToggleClientEnableByEmail(clientEmail string) (bool, bool, error) {
	_, inbound, err := s.GetClientInboundByEmail(clientEmail)
	if err != nil {
		return false, false, err
	}
	if inbound == nil {
		return false, false, common.NewError("Inbound Not Found For Email:", clientEmail)
	}

	oldClients, err := s.GetClients(inbound)
	if err != nil {
		return false, false, err
	}

	clientId := ""
	clientOldEnabled := false

	for _, oldClient := range oldClients {
		if oldClient.Email == clientEmail {
			switch inbound.Protocol {
			case "trojan":
				clientId = oldClient.Password
			case "shadowsocks":
				clientId = oldClient.Email
			default:
				clientId = oldClient.ID
			}
			clientOldEnabled = oldClient.Enable
			break
		}
	}

	if len(clientId) == 0 {
		return false, false, common.NewError("Client Not Found For Email:", clientEmail)
	}

	settings, err := inboundSettingsObject(inbound.Settings, inbound.Id)
	if err != nil {
		return false, false, err
	}
	clients, err := inboundSettingsClients(settings, inbound.Id)
	if err != nil {
		return false, false, err
	}
	var newClients []any
	for client_index := range clients {
		c := clients[client_index].(map[string]any)
		if c["email"] == clientEmail {
			c["enable"] = !clientOldEnabled
			c["updated_at"] = time.Now().Unix() * 1000
			if clientOldEnabled {
				// Toggling a client OFF is a manual disable: record it so no
				// automatic path (quota increase, expiry extension, traffic
				// job, reseller restore) ever re-enables it implicitly.
				c["disableReason"] = DisableReasonManual
			} else {
				// Toggling it back ON clears any recorded reason.
				delete(c, "disableReason")
			}
			newClients = append(newClients, any(c))
		}
	}
	settings["clients"] = newClients
	modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, false, err
	}
	inbound.Settings = string(modifiedSettings)

	needRestart, err := s.UpdateInboundClient(inbound, clientId)
	if err != nil {
		return false, needRestart, err
	}

	return !clientOldEnabled, needRestart, nil
}

// SetClientEnableByEmail sets client enable state to desired value; returns (changed, needRestart, error)
func (s *InboundService) SetClientEnableByEmail(clientEmail string, enable bool) (bool, bool, error) {
	current, err := s.checkIsEnabledByEmail(clientEmail)
	if err != nil {
		return false, false, err
	}
	if current == enable {
		return false, false, nil
	}
	newEnabled, needRestart, err := s.ToggleClientEnableByEmail(clientEmail)
	if err != nil {
		return false, needRestart, err
	}
	return newEnabled == enable, needRestart, nil
}

func (s *InboundService) ResetClientIpLimitByEmail(clientEmail string, count int) (bool, error) {
	if count < 0 {
		return false, common.NewError("IP limit cannot be negative")
	}
	_, inbound, err := s.GetClientInboundByEmail(clientEmail)
	if err != nil {
		return false, err
	}
	if inbound == nil {
		return false, common.NewError("Inbound Not Found For Email:", clientEmail)
	}

	oldClients, err := s.GetClients(inbound)
	if err != nil {
		return false, err
	}

	clientId := ""

	for _, oldClient := range oldClients {
		if oldClient.Email == clientEmail {
			switch inbound.Protocol {
			case "trojan":
				clientId = oldClient.Password
			case "shadowsocks":
				clientId = oldClient.Email
			default:
				clientId = oldClient.ID
			}
			break
		}
	}

	if len(clientId) == 0 {
		return false, common.NewError("Client Not Found For Email:", clientEmail)
	}

	settings, err := inboundSettingsObject(inbound.Settings, inbound.Id)
	if err != nil {
		return false, err
	}
	clients, err := inboundSettingsClients(settings, inbound.Id)
	if err != nil {
		return false, err
	}
	var newClients []any
	for client_index := range clients {
		c := clients[client_index].(map[string]any)
		if c["email"] == clientEmail {
			c["limitIp"] = count
			c["updated_at"] = time.Now().Unix() * 1000
			newClients = append(newClients, any(c))
		}
	}
	settings["clients"] = newClients
	modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	inbound.Settings = string(modifiedSettings)
	needRestart, err := s.UpdateInboundClient(inbound, clientId)
	return needRestart, err
}

func (s *InboundService) ResetClientExpiryTimeByEmail(clientEmail string, expiry_time int64) (bool, error) {
	_, inbound, err := s.GetClientInboundByEmail(clientEmail)
	if err != nil {
		return false, err
	}
	if inbound == nil {
		return false, common.NewError("Inbound Not Found For Email:", clientEmail)
	}

	oldClients, err := s.GetClients(inbound)
	if err != nil {
		return false, err
	}

	clientId := ""

	for _, oldClient := range oldClients {
		if oldClient.Email == clientEmail {
			switch inbound.Protocol {
			case "trojan":
				clientId = oldClient.Password
			case "shadowsocks":
				clientId = oldClient.Email
			default:
				clientId = oldClient.ID
			}
			break
		}
	}

	if len(clientId) == 0 {
		return false, common.NewError("Client Not Found For Email:", clientEmail)
	}

	settings, err := inboundSettingsObject(inbound.Settings, inbound.Id)
	if err != nil {
		return false, err
	}
	clients, err := inboundSettingsClients(settings, inbound.Id)
	if err != nil {
		return false, err
	}
	var newClients []any
	for client_index := range clients {
		c := clients[client_index].(map[string]any)
		if c["email"] == clientEmail {
			c["expiryTime"] = expiry_time
			c["updated_at"] = time.Now().Unix() * 1000
			newClients = append(newClients, any(c))
		}
	}
	settings["clients"] = newClients
	modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	inbound.Settings = string(modifiedSettings)
	needRestart, err := s.UpdateInboundClient(inbound, clientId)
	return needRestart, err
}

func (s *InboundService) ResetClientTrafficLimitByEmail(clientEmail string, totalGB int) (bool, error) {
	if totalGB < 0 {
		return false, common.NewError("totalGB must be >= 0")
	}
	_, inbound, err := s.GetClientInboundByEmail(clientEmail)
	if err != nil {
		return false, err
	}
	if inbound == nil {
		return false, common.NewError("Inbound Not Found For Email:", clientEmail)
	}

	oldClients, err := s.GetClients(inbound)
	if err != nil {
		return false, err
	}

	clientId := ""

	for _, oldClient := range oldClients {
		if oldClient.Email == clientEmail {
			switch inbound.Protocol {
			case "trojan":
				clientId = oldClient.Password
			case "shadowsocks":
				clientId = oldClient.Email
			default:
				clientId = oldClient.ID
			}
			break
		}
	}

	if len(clientId) == 0 {
		return false, common.NewError("Client Not Found For Email:", clientEmail)
	}

	settings, err := inboundSettingsObject(inbound.Settings, inbound.Id)
	if err != nil {
		return false, err
	}
	clients, err := inboundSettingsClients(settings, inbound.Id)
	if err != nil {
		return false, err
	}
	var newClients []any
	for client_index := range clients {
		c := clients[client_index].(map[string]any)
		if c["email"] == clientEmail {
			c["totalGB"] = totalGB * 1024 * 1024 * 1024
			c["updated_at"] = time.Now().Unix() * 1000
			newClients = append(newClients, any(c))
		}
	}
	settings["clients"] = newClients
	modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	inbound.Settings = string(modifiedSettings)
	needRestart, err := s.UpdateInboundClient(inbound, clientId)
	return needRestart, err
}

// ResetClientTrafficByEmail resets a client's usage counters. For a client
// automatically disabled by quota depletion the reset resolves the condition,
// so the client is re-enabled (the same semantics as increasing its volume).
// A manually disabled, reseller-held or expired client is reset but stays
// disabled, keeping client_traffics consistent with the settings.
func (s *InboundService) ResetClientTrafficByEmail(clientEmail string) (bool, error) {
	needRestart := false
	traffic, inbound, err := s.GetClientInboundByEmail(clientEmail)
	if err != nil {
		return false, err
	}
	if inbound == nil {
		return false, common.NewError("Inbound Not Found For Email:", clientEmail)
	}

	oldClients, err := s.GetClients(inbound)
	if err != nil {
		return false, err
	}
	var client *model.Client
	clientId := ""
	for i := range oldClients {
		if oldClients[i].Email == clientEmail {
			client = &oldClients[i]
			switch inbound.Protocol {
			case "trojan":
				clientId = client.Password
			case "shadowsocks":
				clientId = client.Email
			default:
				clientId = client.ID
			}
			break
		}
	}
	if client == nil {
		// No settings client (orphaned stats row): reset the usage only.
		return false, s.resetClientUsageStats(clientEmail, nil)
	}
	resetQuota := model.InitialClientTrafficQuota(*client)
	if client.Enable {
		// Enabled client: a plain usage reset, re-aligning the stats row with
		// the settings so a legacy enable disagreement cannot survive.
		return false, database.GetDB().Model(xray.ClientTraffic{}).
			Where("email = ?", clientEmail).
			Updates(map[string]any{"up": 0, "down": 0, "total": resetQuota, "enable": true}).Error
	}

	reason := classifyDisableReason(client.DisableReason, "")
	if reason == DisableReasonManual || reason == DisableReasonReseller {
		// The disable was manual (or a reseller lifecycle hold): reset the
		// usage but never re-enable, and keep the stats row disabled so it
		// stays consistent with the settings.
		return false, s.resetClientUsageStats(clientEmail, &resetQuota)
	}
	if reason == "" {
		// Legacy disable without a recorded reason: only re-enable when the
		// old-limits heuristic says the disable was automatic (usage had
		// reached the volume) and the client is not expired.
		usage := int64(0)
		if traffic != nil {
			usage = traffic.Up + traffic.Down
		}
		now := time.Now().Unix() * 1000
		wasDepleted := traffic != nil && traffic.Total > 0 && usage >= traffic.Total
		wasExpired := client.ExpiryTime > 0 && client.ExpiryTime <= now
		if !wasDepleted || wasExpired {
			return false, s.resetClientUsageStats(clientEmail, &resetQuota)
		}
	}

	// Auto-disabled: zero the usage first, then check that the client's
	// cohort would no longer be depleted (expiry and sibling usage included).
	if err := s.resetClientUsageStats(clientEmail, &resetQuota); err != nil {
		return false, err
	}
	depleted, err := s.cohortStillDepletedForClient(clientEmail, client)
	if err != nil {
		return false, err
	}
	if depleted {
		return false, nil
	}

	// Re-enable through UpdateInboundClient so the settings, the
	// client_traffics row and the Xray runtime stay consistent and the
	// disable marker is cleared.
	settings, err := inboundSettingsObject(inbound.Settings, inbound.Id)
	if err != nil {
		return false, err
	}
	clientsRaw, err := inboundSettingsClients(settings, inbound.Id)
	if err != nil {
		return false, err
	}
	for _, rawClient := range clientsRaw {
		cm, ok := rawClient.(map[string]any)
		if !ok {
			continue
		}
		if cm["email"] != clientEmail {
			continue
		}
		cm["enable"] = true
		delete(cm, "disableReason")
		cm["updated_at"] = time.Now().Unix() * 1000
	}
	settings["clients"] = clientsRaw
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	inbound.Settings = string(body)
	needRestart, err = s.UpdateInboundClient(inbound, clientId)
	return needRestart, err
}

// resetClientUsageStats zeroes a client's usage counters without touching its
// enable state, so client_traffics stays consistent with the settings.
func (s *InboundService) resetClientUsageStats(clientEmail string, quotaTotal *int64) error {
	updates := map[string]any{"up": 0, "down": 0}
	if quotaTotal != nil {
		updates["total"] = *quotaTotal
	}
	return database.GetDB().Model(xray.ClientTraffic{}).
		Where("email = ?", clientEmail).
		Updates(updates).Error
}

func (s *InboundService) ResetClientTraffic(id int, clientEmail string) (bool, error) {
	inbound, err := s.GetInbound(id)
	if err != nil {
		return false, err
	}
	if inbound.NodeID != nil {
		node, remoteID, err := s.getInboundNode(inbound)
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := (&NodeService{}).ResetRemoteClientTraffic(ctx, node, remoteID, clientEmail); err != nil {
			return false, err
		}
		_, err = (&NodeService{}).SyncNode(ctx, node.Id)
		return false, err
	}

	// Local inbound: reuse the by-email reset so the auto/manual disable
	// semantics and the settings/stats/runtime consistency are identical for
	// both entry points (a quota-depleted client is re-enabled; a manual,
	// reseller-held or expired one is reset but stays disabled).
	return s.ResetClientTrafficByEmail(clientEmail)
}

func (s *InboundService) ResetAllClientTraffics(id int) error {
	if id != -1 {
		inbound, err := s.GetInbound(id)
		if err != nil {
			return err
		}
		if inbound.NodeID != nil {
			node, remoteID, err := s.getInboundNode(inbound)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := (&NodeService{}).ResetRemoteAllClientTraffic(ctx, node, remoteID); err != nil {
				return err
			}
			_, err = (&NodeService{}).SyncNode(ctx, node.Id)
			return err
		}
	}

	db := database.GetDB()
	now := time.Now().Unix() * 1000

	return db.Transaction(func(tx *gorm.DB) error {
		whereText := "inbound_id "
		if id == -1 {
			whereText += " > ?"
		} else {
			whereText += " = ?"
		}

		inboundWhereText := "id "
		if id == -1 {
			inboundWhereText += " > ?"
		} else {
			inboundWhereText += " = ?"
		}
		var inbounds []model.Inbound
		if err := tx.Where(inboundWhereText, id).Find(&inbounds).Error; err != nil {
			return err
		}
		quotaByEmail := make(map[string]int64)
		for _, inbound := range inbounds {
			var settings struct {
				Clients []model.Client `json:"clients"`
			}
			if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
				return fmt.Errorf("decode clients for inbound %d during traffic reset: %w", inbound.Id, err)
			}
			for _, client := range settings.Clients {
				if email := strings.ToLower(strings.TrimSpace(client.Email)); email != "" {
					quotaByEmail[email] = model.InitialClientTrafficQuota(client)
				}
			}
		}

		var traffics []xray.ClientTraffic
		if err := tx.Where(whereText, id).Find(&traffics).Error; err != nil {
			return err
		}
		for _, traffic := range traffics {
			updates := map[string]any{"up": 0, "down": 0}
			if quota, ok := quotaByEmail[strings.ToLower(strings.TrimSpace(traffic.Email))]; ok {
				updates["total"] = quota
			}
			if err := tx.Model(xray.ClientTraffic{}).
				Where("id = ?", traffic.Id).
				Updates(updates).Error; err != nil {
				return err
			}
		}

		// Update lastTrafficResetTime for the inbound(s)
		result := tx.Model(model.Inbound{}).
			Where(inboundWhereText, id).
			Update("last_traffic_reset_time", now)

		return result.Error
	})
}

func (s *InboundService) ResetAllTraffics() error {
	db := database.GetDB()

	result := db.Model(model.Inbound{}).
		Where("user_id > ?", 0).
		Updates(map[string]any{"up": 0, "down": 0})

	err := result.Error
	return err
}

func (s *InboundService) ResetInboundTraffic(id int) error {
	db := database.GetDB()

	result := db.Model(model.Inbound{}).
		Where("id = ?", id).
		Updates(map[string]any{"up": 0, "down": 0})

	return result.Error
}

func (s *InboundService) DelDepletedClients(id int) (err error) {
	db := database.GetDB()
	tx := db.Begin()
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()

	now := time.Now().Unix() * 1000
	cohorts, err := s.depletedQuotaCohorts(tx, now)
	if err != nil {
		return err
	}
	depletedEmails := make(map[string]struct{})
	for _, cohort := range cohorts {
		if !cohort.Depleted {
			continue
		}
		for email := range cohort.Emails {
			depletedEmails[email] = struct{}{}
		}
	}
	if len(depletedEmails) == 0 {
		return nil
	}

	var inbounds []*model.Inbound
	inboundQuery := db.Model(model.Inbound{})
	if id >= 0 {
		inboundQuery = inboundQuery.Where("id = ?", id)
	}
	if err = inboundQuery.Find(&inbounds).Error; err != nil {
		return err
	}

	for _, inbound := range inbounds {
		var settings map[string]any
		err = json.Unmarshal([]byte(inbound.Settings), &settings)
		if err != nil {
			return err
		}
		rawClients, ok := settings["clients"].([]any)
		if !ok {
			continue
		}
		newClients := make([]any, 0, len(rawClients))
		removed := 0
		for _, client := range rawClients {
			c, ok := client.(map[string]any)
			if !ok {
				newClients = append(newClients, client)
				continue
			}
			email, _ := c["email"].(string)
			if _, isDepleted := depletedEmails[strings.ToLower(email)]; isDepleted {
				removed++
				continue
			}
			newClients = append(newClients, client)
		}
		if removed == 0 {
			continue
		}
		if len(newClients) == 0 {
			if _, err = s.DelInbound(inbound.Id); err != nil {
				return err
			}
			continue
		}
		settings["clients"] = newClients
		newSettings, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return err
		}
		inbound.Settings = string(newSettings)
		if err = tx.Save(inbound).Error; err != nil {
			return err
		}
	}

	if id < 0 {
		return tx.Where("reset = 0 and ((total > 0 and up + down >= total) or (expiry_time > 0 and expiry_time <= ?))", now).Delete(xray.ClientTraffic{}).Error
	}

	toDelete := make([]string, 0, len(depletedEmails))
	for email := range depletedEmails {
		ids, err := s.getInboundIDsForEmail(tx, email)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			toDelete = append(toDelete, email)
		}
	}
	if len(toDelete) > 0 {
		err = tx.Where("LOWER(email) IN ?", toDelete).Delete(xray.ClientTraffic{}).Error
	}

	return nil
}

func (s *InboundService) GetClientTrafficTgBot(tgId int64) ([]*xray.ClientTraffic, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound

	// Retrieve inbounds where settings contain the given tgId
	err := db.Model(model.Inbound{}).Where("settings LIKE ?", fmt.Sprintf(`%%"tgId": %d%%`, tgId)).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		logger.Errorf("Error retrieving inbounds with tgId %d: %v", tgId, err)
		return nil, err
	}

	var emails []string
	for _, inbound := range inbounds {
		clients, err := s.GetClients(inbound)
		if err != nil {
			logger.Errorf("Error retrieving clients for inbound %d: %v", inbound.Id, err)
			continue
		}
		for _, client := range clients {
			if client.TgID == tgId {
				emails = append(emails, client.Email)
			}
		}
	}

	var traffics []*xray.ClientTraffic
	err = db.Model(xray.ClientTraffic{}).Where("email IN ?", emails).Find(&traffics).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			logger.Warning("No ClientTraffic records found for emails:", emails)
			return nil, nil
		}
		logger.Errorf("Error retrieving ClientTraffic for emails %v: %v", emails, err)
		return nil, err
	}

	// Populate UUID and other client data for each traffic record
	for i := range traffics {
		if ct, client, e := s.GetClientByEmail(traffics[i].Email); e == nil && ct != nil && client != nil {
			traffics[i].Enable = client.Enable
			traffics[i].UUID = client.ID
			traffics[i].SubId = client.SubID
		}
	}

	return traffics, nil
}

func (s *InboundService) GetClientTrafficByEmail(email string) (traffic *xray.ClientTraffic, err error) {
	// Prefer retrieving along with client to reflect actual enabled state from inbound settings
	t, client, err := s.GetClientByEmail(email)
	if err != nil {
		logger.Warningf("Error retrieving ClientTraffic with email %s: %v", email, err)
		return nil, err
	}
	if t != nil && client != nil {
		t.UUID = client.ID
		t.SubId = client.SubID
		return t, nil
	}
	return nil, nil
}

func (s *InboundService) UpdateClientTrafficByEmail(email string, upload int64, download int64) error {
	db := database.GetDB()

	result := db.Model(xray.ClientTraffic{}).
		Where("email = ?", email).
		Updates(map[string]any{"up": upload, "down": download})

	err := result.Error
	if err != nil {
		logger.Warningf("Error updating ClientTraffic with email %s: %v", email, err)
		return err
	}
	return nil
}

func (s *InboundService) GetClientTrafficByID(id string) ([]xray.ClientTraffic, error) {
	db := database.GetDB()
	var traffics []xray.ClientTraffic
	ids := make(map[string]struct{})
	for _, part := range strings.Split(id, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			ids[part] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return traffics, nil
	}

	inbounds, err := s.GetAllInbounds()
	if err != nil {
		logger.Debug(err)
		return nil, err
	}
	emails := make([]string, 0)
	for _, inbound := range inbounds {
		clients, err := s.GetClients(inbound)
		if err != nil {
			return nil, err
		}
		for _, client := range clients {
			if _, ok := ids[client.ID]; ok && client.Email != "" {
				emails = append(emails, client.Email)
			}
		}
	}
	if len(emails) == 0 {
		return traffics, nil
	}

	err = db.Model(xray.ClientTraffic{}).Where("email IN ?", emails).Find(&traffics).Error

	if err != nil {
		logger.Debug(err)
		return nil, err
	}
	// Reconcile enable flag with client settings per email to avoid stale DB value
	for i := range traffics {
		if ct, client, e := s.GetClientByEmail(traffics[i].Email); e == nil && ct != nil && client != nil {
			traffics[i].Enable = client.Enable
			traffics[i].UUID = client.ID
			traffics[i].SubId = client.SubID
		}
	}
	return traffics, err
}

func (s *InboundService) SearchClientTraffic(query string) (traffic *xray.ClientTraffic, err error) {
	db := database.GetDB()
	inbound := &model.Inbound{}
	traffic = &xray.ClientTraffic{}

	// Search for inbound settings that contain the query
	err = db.Model(model.Inbound{}).Where("settings LIKE ?", "%\""+query+"\"%").First(inbound).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			logger.Warningf("Inbound settings containing query %s not found: %v", query, err)
			return nil, err
		}
		logger.Errorf("Error searching for inbound settings with query %s: %v", query, err)
		return nil, err
	}

	traffic.InboundId = inbound.Id

	// Unmarshal settings to get clients. A struct (not map[string][]model.Client)
	// so extra top-level keys like vless "decryption" don't fail the parse.
	var settings struct {
		Clients []model.Client `json:"clients"`
	}
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		logger.Errorf("Error unmarshalling inbound settings for inbound ID %d: %v", inbound.Id, err)
		return nil, err
	}

	for _, client := range settings.Clients {
		if (client.ID == query || client.Password == query) && client.Email != "" {
			traffic.Email = client.Email
			break
		}
	}

	if traffic.Email == "" {
		logger.Warningf("No client found with query %s in inbound ID %d", query, inbound.Id)
		return nil, gorm.ErrRecordNotFound
	}

	// Retrieve ClientTraffic based on the found email
	err = db.Model(xray.ClientTraffic{}).Where("email = ?", traffic.Email).First(traffic).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			logger.Warningf("ClientTraffic for email %s not found: %v", traffic.Email, err)
			return nil, err
		}
		logger.Errorf("Error retrieving ClientTraffic for email %s: %v", traffic.Email, err)
		return nil, err
	}

	return traffic, nil
}

func (s *InboundService) GetInboundClientIps(clientEmail string) (string, error) {
	// The IP limit engine tracks live IPs in memory and never writes them to
	// the database per event; when it has seen the client, its view is
	// authoritative and returned in the same [{ip,timestamp}] format.
	if live := clientLimitEngineLiveIPs(clientEmail); live != nil {
		if result, err := json.Marshal(live); err == nil {
			return string(result), nil
		}
	}

	db := database.GetDB()
	InboundClientIps := &model.InboundClientIps{}
	err := db.Model(model.InboundClientIps{}).Where("client_email = ?", clientEmail).First(InboundClientIps).Error
	if err != nil {
		return "", err
	}

	if InboundClientIps.Ips == "" {
		return "", nil
	}

	// Try to parse as new format (with timestamps)
	type IPWithTimestamp struct {
		IP        string `json:"ip"`
		Timestamp int64  `json:"timestamp"`
	}

	var ipsWithTime []IPWithTimestamp
	err = json.Unmarshal([]byte(InboundClientIps.Ips), &ipsWithTime)

	// If successfully parsed as new format, return with timestamps
	if err == nil && len(ipsWithTime) > 0 {
		return InboundClientIps.Ips, nil
	}

	// Otherwise, assume it's old format (simple string array)
	// Try to parse as simple array and convert to new format
	var oldIps []string
	err = json.Unmarshal([]byte(InboundClientIps.Ips), &oldIps)
	if err == nil && len(oldIps) > 0 {
		// Convert old format to new format with current timestamp
		newIpsWithTime := make([]IPWithTimestamp, len(oldIps))
		for i, ip := range oldIps {
			newIpsWithTime[i] = IPWithTimestamp{
				IP:        ip,
				Timestamp: time.Now().Unix(),
			}
		}
		result, _ := json.Marshal(newIpsWithTime)
		return string(result), nil
	}

	// Return as-is if parsing fails
	return InboundClientIps.Ips, nil
}

func (s *InboundService) ClearClientIps(clientEmail string) error {
	db := database.GetDB()

	result := db.Model(model.InboundClientIps{}).
		Where("client_email = ?", clientEmail).
		Update("ips", "")
	err := result.Error
	if err != nil {
		return err
	}
	// Also forget the engine's live state so clearing takes effect at once.
	clientLimitEngineReset(clientEmail)
	return nil
}

func (s *InboundService) SearchInbounds(query string) ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Preload("ClientStats").Where("remark like ?", "%"+query+"%").Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return inbounds, nil
}

func (s *InboundService) MigrationRequirements() {
	db := database.GetDB()
	tx := db.Begin()
	var err error
	defer func() {
		if err == nil {
			tx.Commit()
			if dbErr := db.Exec(`VACUUM "main"`).Error; dbErr != nil {
				logger.Warningf("VACUUM failed: %v", dbErr)
			}
		} else {
			tx.Rollback()
		}
	}()

	// Calculate and backfill all_time from up+down for inbounds and clients
	err = tx.Exec(`
		UPDATE inbounds
		SET all_time = IFNULL(up, 0) + IFNULL(down, 0)
		WHERE IFNULL(all_time, 0) = 0 AND (IFNULL(up, 0) + IFNULL(down, 0)) > 0
	`).Error
	if err != nil {
		return
	}
	err = tx.Exec(`
		UPDATE client_traffics
		SET all_time = IFNULL(up, 0) + IFNULL(down, 0)
		WHERE IFNULL(all_time, 0) = 0 AND (IFNULL(up, 0) + IFNULL(down, 0)) > 0
	`).Error

	if err != nil {
		return
	}

	// Fix inbounds based problems
	var inbounds []*model.Inbound
	err = tx.Model(model.Inbound{}).Where("protocol IN (?)", []string{"vmess", "vless", "trojan"}).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return
	}
	for inbound_index := range inbounds {
		settings := map[string]any{}
		json.Unmarshal([]byte(inbounds[inbound_index].Settings), &settings)
		clients, ok := settings["clients"].([]any)
		if ok {
			// Fix Client configuration problems
			var newClients []any
			for client_index := range clients {
				c := clients[client_index].(map[string]any)

				// Add email='' if it is not exists
				if _, ok := c["email"]; !ok {
					c["email"] = ""
				}

				// Convert string tgId to int64
				if _, ok := c["tgId"]; ok {
					var tgId any = c["tgId"]
					if tgIdStr, ok2 := tgId.(string); ok2 {
						tgIdInt64, err := strconv.ParseInt(strings.ReplaceAll(tgIdStr, " ", ""), 10, 64)
						if err == nil {
							c["tgId"] = tgIdInt64
						}
					}
				}

				// Remove "flow": "xtls-rprx-direct"
				if _, ok := c["flow"]; ok {
					if c["flow"] == "xtls-rprx-direct" {
						c["flow"] = ""
					}
				}
				// Backfill created_at and updated_at
				if _, ok := c["created_at"]; !ok {
					c["created_at"] = time.Now().Unix() * 1000
				}
				c["updated_at"] = time.Now().Unix() * 1000
				newClients = append(newClients, any(c))
			}
			settings["clients"] = newClients
			modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				return
			}

			inbounds[inbound_index].Settings = string(modifiedSettings)
		}

		// Add client traffic row for all clients which has email
		modelClients, err := s.GetClients(inbounds[inbound_index])
		if err != nil {
			return
		}
		for _, modelClient := range modelClients {
			if len(modelClient.Email) > 0 {
				var count int64
				tx.Model(xray.ClientTraffic{}).Where("email = ?", modelClient.Email).Count(&count)
				if count == 0 {
					s.AddClientStat(tx, inbounds[inbound_index].Id, &modelClient)
				}
			}
		}
	}
	tx.Save(inbounds)

	// Remove orphaned traffics
	tx.Where("inbound_id = 0").Delete(xray.ClientTraffic{})

	// Migrate old MultiDomain to External Proxy
	var externalProxy []struct {
		Id             int
		Port           int
		StreamSettings []byte
	}
	var candidates []model.Inbound
	err = tx.Model(model.Inbound{}).Select("id", "port", "stream_settings").Where("protocol IN ?", []string{"vmess", "vless", "trojan"}).Find(&candidates).Error
	if err != nil {
		return
	}
	for _, inbound := range candidates {
		var stream map[string]any
		if err := json.Unmarshal([]byte(inbound.StreamSettings), &stream); err != nil {
			continue
		}
		if stream["security"] != "tls" {
			continue
		}
		tlsSettings, ok := stream["tlsSettings"].(map[string]any)
		if !ok {
			continue
		}
		settings, ok := tlsSettings["settings"].(map[string]any)
		if !ok {
			continue
		}
		domains, ok := settings["domains"].([]any)
		if !ok || len(domains) == 0 {
			continue
		}
		externalProxy = append(externalProxy, struct {
			Id             int
			Port           int
			StreamSettings []byte
		}{
			Id:             inbound.Id,
			Port:           inbound.Port,
			StreamSettings: []byte(inbound.StreamSettings),
		})
	}
	if len(externalProxy) == 0 {
		return
	}

	for _, ep := range externalProxy {
		var reverses any
		var stream map[string]any
		json.Unmarshal(ep.StreamSettings, &stream)
		if tlsSettings, ok := stream["tlsSettings"].(map[string]any); ok {
			if settings, ok := tlsSettings["settings"].(map[string]any); ok {
				if domains, ok := settings["domains"].([]any); ok {
					for _, domain := range domains {
						if domainMap, ok := domain.(map[string]any); ok {
							domainMap["forceTls"] = "same"
							domainMap["port"] = ep.Port
							domainMap["dest"] = domainMap["domain"].(string)
							delete(domainMap, "domain")
						}
					}
				}
				reverses = settings["domains"]
				delete(settings, "domains")
			}
		}
		stream["externalProxy"] = reverses
		newStream, _ := json.MarshalIndent(stream, " ", "  ")
		tx.Model(model.Inbound{}).Where("id = ?", ep.Id).Update("stream_settings", newStream)
	}

	err = tx.Raw(`UPDATE inbounds
	SET tag = REPLACE(tag, '0.0.0.0:', '')
	WHERE INSTR(tag, '0.0.0.0:') > 0;`).Error
	if err != nil {
		return
	}
}

func (s *InboundService) MigrateDB() {
	s.MigrationRequirements()
	s.MigrationRemoveOrphanedTraffics()
}

func (s *InboundService) GetOnlineClients() []string {
	return p.GetOnlineClients()
}

func (s *InboundService) GetClientsLastOnline() (map[string]int64, error) {
	db := database.GetDB()
	var rows []xray.ClientTraffic
	err := db.Model(&xray.ClientTraffic{}).Select("email, last_online").Find(&rows).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	result := make(map[string]int64, len(rows))
	for _, r := range rows {
		result[r.Email] = r.LastOnline
	}
	return result, nil
}

func (s *InboundService) FilterAndSortClientEmails(emails []string) ([]string, []string, error) {
	db := database.GetDB()

	// Step 1: Get ClientTraffic records for emails in the input list
	var clients []xray.ClientTraffic
	err := db.Where("email IN ?", emails).Find(&clients).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, nil, err
	}

	// Step 2: Sort clients by (Up + Down) descending
	sort.Slice(clients, func(i, j int) bool {
		return (clients[i].Up + clients[i].Down) > (clients[j].Up + clients[j].Down)
	})

	// Step 3: Extract sorted valid emails and track found ones
	validEmails := make([]string, 0, len(clients))
	found := make(map[string]bool)
	for _, client := range clients {
		validEmails = append(validEmails, client.Email)
		found[client.Email] = true
	}

	// Step 4: Identify emails that were not found in the database
	extraEmails := make([]string, 0)
	for _, email := range emails {
		if !found[email] {
			extraEmails = append(extraEmails, email)
		}
	}

	return validEmails, extraEmails, nil
}
func (s *InboundService) DelInboundClientByEmail(inboundId int, email string) (bool, error) {
	defer ClientLimitGateRefresh()
	oldInbound, err := s.GetInbound(inboundId)
	if err != nil {
		logger.Error("Load Old Data Error")
		return false, err
	}
	if oldInbound.NodeID != nil {
		node, remoteID, err := s.getInboundNode(oldInbound)
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := (&NodeService{}).DeleteRemoteClientByEmail(ctx, node, remoteID, email); err != nil {
			return false, err
		}
		_, err = (&NodeService{}).SyncNode(ctx, node.Id)
		return false, err
	}

	settings, err := inboundSettingsObject(oldInbound.Settings, oldInbound.Id)
	if err != nil {
		return false, err
	}

	interfaceClients, err := inboundSettingsClients(settings, oldInbound.Id)
	if err != nil {
		return false, err
	}

	var newClients []any
	needApiDel := false
	found := false

	for _, client := range interfaceClients {
		c, ok := client.(map[string]any)
		if !ok {
			continue
		}
		if cEmail, ok := c["email"].(string); ok && cEmail == email {
			// matched client, drop it
			found = true
			needApiDel, _ = c["enable"].(bool)
		} else {
			newClients = append(newClients, client)
		}
	}

	if !found {
		return false, common.NewError(fmt.Sprintf("client with email %s not found", email))
	}
	settings["clients"] = newClients
	newSettings, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}

	oldInbound.Settings = string(newSettings)

	db := database.GetDB()

	// remove IP bindings
	if err := s.DelClientIPs(db, email); err != nil {
		logger.Error("Error in delete client IPs")
		return false, err
	}

	needRestart := false

	// remove stats too
	if len(email) > 0 {
		traffic, err := s.GetClientTrafficByEmail(email)
		if err != nil {
			return false, err
		}
		if traffic != nil {
			if err := s.DelClientStat(db, email); err != nil {
				logger.Error("Delete stats Data Error")
				return false, err
			}
		}

		if needApiDel {
			// If the Xray process is not running, skip the runtime API call;
			// the database state stays authoritative and a rebuild on the next
			// start reflects the deletion.
			if p == nil {
				needRestart = true
			} else {
				s.xrayApi.Init(p.GetAPIPort())
				if err1 := s.xrayApi.RemoveUser(oldInbound.Tag, email); err1 == nil {
					logger.Debug("Client deleted by api:", email)
					needRestart = false
				} else {
					if strings.Contains(err1.Error(), fmt.Sprintf("User %s not found.", email)) {
						logger.Debug("User is already deleted. Nothing to do more...")
					} else {
						logger.Debug("Error in deleting client by api:", err1)
						needRestart = true
					}
				}
				s.xrayApi.Close()
			}
		}
	}

	return needRestart, db.Save(oldInbound).Error
}
