package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/util/common"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

type MultiLinkService struct {
	inboundService InboundService
	settingService SettingService
}

type MultiLinkRow struct {
	SubID        string   `json:"subId"`
	ClientCount  int      `json:"clientCount"`
	Emails       []string `json:"emails"`
	InboundIDs   []int    `json:"inboundIds"`
	InboundTags  []string `json:"inboundTags"`
	Used         int64    `json:"used"`
	Total        int64    `json:"total"`
	NominalTotal int64    `json:"nominalTotal"`
	QuotaFactor  int64    `json:"quotaFactor"`
	LimitIP      int      `json:"limitIp"`
	Remaining    int64    `json:"remaining"`
	ExpiryTime   int64    `json:"expiryTime"`
	Reset        int      `json:"reset"`
	Enabled      bool     `json:"enabled"`
	Online       bool     `json:"online"`
	Finished     bool     `json:"finished"`
	Status       string   `json:"status"`
	LastOnline   int64    `json:"lastOnline"`
	SubURL       string   `json:"subUrl"`
	UsagePct     float64  `json:"usagePct"`
	Watching     bool     `json:"watching"`
	StatusLabel  string   `json:"statusLabel"`
}

type MultiLinkUpdate struct {
	TotalGB      int64  `json:"totalGB" form:"totalGB"`
	QuotaFactor  *int64 `json:"quotaFactor" form:"quotaFactor"`
	LimitIP      *int   `json:"limitIp" form:"limitIp"`
	ExpiryTime   int64  `json:"expiryTime" form:"expiryTime"`
	Reset        int    `json:"reset" form:"reset"`
	Enable       bool   `json:"enable" form:"enable"`
	ResetTraffic bool   `json:"resetTraffic" form:"resetTraffic"`
}

type multiLinkTarget struct {
	Inbound  *model.Inbound
	Client   model.Client
	ClientID string
	Traffic  *xray.ClientTraffic
}

func multiLinkClientID(protocol model.Protocol, client model.Client) string {
	switch protocol {
	case model.Trojan:
		return client.Password
	case model.Shadowsocks:
		return client.Email
	case model.Hysteria:
		return client.Auth
	default:
		return client.ID
	}
}

func (s *MultiLinkService) buildSubURL(subID string) string {
	if subID == "" {
		return ""
	}
	if configured, _ := s.settingService.GetSubURI(); strings.TrimSpace(configured) != "" {
		if strings.HasSuffix(configured, "/") {
			return configured + url.PathEscape(subID)
		}
		return configured + "/" + url.PathEscape(subID)
	}
	if allSetting, err := s.settingService.GetAllSetting(); err == nil {
		configured := strings.TrimSpace(allSetting.SubURI)
		if configured != "" {
			if strings.HasSuffix(configured, "/") {
				return configured + url.PathEscape(subID)
			}
			return configured + "/" + url.PathEscape(subID)
		}
	}
	subDomain, _ := s.settingService.GetSubDomain()
	subPort, _ := s.settingService.GetSubPort()
	subPath, _ := s.settingService.GetSubPath()
	subKeyFile, _ := s.settingService.GetSubKeyFile()
	subCertFile, _ := s.settingService.GetSubCertFile()
	webDomain, _ := s.settingService.GetWebDomain()
	host := strings.TrimSpace(subDomain)
	if host == "" {
		host = strings.TrimSpace(webDomain)
	}
	if host == "" {
		return ""
	}
	scheme := "http"
	if subKeyFile != "" && subCertFile != "" {
		scheme = "https"
	}
	basePath := strings.TrimSpace(subPath)
	if basePath == "" {
		basePath = "/sub/"
	}
	base := fmt.Sprintf("%s://%s:%d%s", scheme, host, subPort, basePath)
	if strings.HasSuffix(base, "/") {
		return base + url.PathEscape(subID)
	}
	return base + "/" + url.PathEscape(subID)
}

func (s *MultiLinkService) collectTargets(userID *int) (map[string][]multiLinkTarget, error) {
	var (
		inbounds []*model.Inbound
		user     *model.User
		err      error
	)
	if userID != nil {
		user, err = (&UserService{}).GetUserByID(*userID)
		if err != nil {
			return nil, err
		}
		inbounds, err = (&UserService{}).GetAccessibleInbounds(user)
	} else {
		inbounds, err = s.inboundService.GetAllInbounds()
	}
	if err != nil {
		return nil, err
	}
	grouped := make(map[string][]multiLinkTarget)
	for _, inbound := range inbounds {
		clients, err := s.inboundService.GetClients(inbound)
		if err != nil {
			continue
		}
		statsByEmail := make(map[string]*xray.ClientTraffic, len(inbound.ClientStats))
		for i := range inbound.ClientStats {
			stat := inbound.ClientStats[i]
			statCopy := stat
			statsByEmail[strings.ToLower(stat.Email)] = &statCopy
		}
		for _, client := range clients {
			if userID != nil && !ClientOwnedByReseller(client, user) {
				continue
			}
			subID := strings.TrimSpace(client.SubID)
			if subID == "" {
				continue
			}
			emailKey := strings.ToLower(strings.TrimSpace(client.Email))
			grouped[subID] = append(grouped[subID], multiLinkTarget{
				Inbound:  inbound,
				Client:   client,
				ClientID: multiLinkClientID(inbound.Protocol, client),
				Traffic:  statsByEmail[emailKey],
			})
		}
	}
	filtered := make(map[string][]multiLinkTarget)
	for subID, targets := range grouped {
		latestPerInbound := map[int]multiLinkTarget{}
		inboundSet := map[int]struct{}{}
		for _, target := range targets {
			inboundSet[target.Inbound.Id] = struct{}{}
			current, ok := latestPerInbound[target.Inbound.Id]
			if !ok || target.Client.UpdatedAt > current.Client.UpdatedAt || target.Client.CreatedAt > current.Client.CreatedAt {
				latestPerInbound[target.Inbound.Id] = target
			}
		}
		if len(inboundSet) > 1 {
			uniqueTargets := make([]multiLinkTarget, 0, len(latestPerInbound))
			for _, target := range latestPerInbound {
				uniqueTargets = append(uniqueTargets, target)
			}
			sort.Slice(uniqueTargets, func(i, j int) bool {
				if uniqueTargets[i].Inbound.Id == uniqueTargets[j].Inbound.Id {
					return strings.ToLower(uniqueTargets[i].Client.Email) < strings.ToLower(uniqueTargets[j].Client.Email)
				}
				return uniqueTargets[i].Inbound.Id < uniqueTargets[j].Inbound.Id
			})
			filtered[subID] = uniqueTargets
		}
	}
	return filtered, nil
}

func (s *MultiLinkService) ListByUser(userID *int) ([]*MultiLinkRow, error) {
	grouped, err := s.collectTargets(userID)
	if err != nil {
		return nil, err
	}
	onlineSet := map[string]struct{}{}
	for _, email := range s.inboundService.GetOnlineClients() {
		onlineSet[strings.ToLower(email)] = struct{}{}
	}
	lastOnlineMap, _ := s.inboundService.GetClientsLastOnline()
	rows := make([]*MultiLinkRow, 0, len(grouped))
	now := time.Now().UnixMilli()
	for subID, targets := range grouped {
		row := &MultiLinkRow{
			SubID:       subID,
			Emails:      make([]string, 0, len(targets)),
			InboundIDs:  make([]int, 0, len(targets)),
			InboundTags: make([]string, 0, len(targets)),
			Enabled:     true,
			Status:      "active",
			StatusLabel: "active",
			SubURL:      s.buildSubURL(subID),
		}
		seenInbound := map[int]struct{}{}
		seenTag := map[string]struct{}{}
		totalLimit := int64(0)
		nominalLimit := int64(0)
		quotaFactor := int64(0)
		limitIP := 0
		lastOnline := int64(0)
		for _, target := range targets {
			row.ClientCount++
			row.Emails = append(row.Emails, target.Client.Email)
			if _, ok := seenInbound[target.Inbound.Id]; !ok {
				seenInbound[target.Inbound.Id] = struct{}{}
				row.InboundIDs = append(row.InboundIDs, target.Inbound.Id)
			}
			if _, ok := seenTag[target.Inbound.Tag]; !ok {
				seenTag[target.Inbound.Tag] = struct{}{}
				row.InboundTags = append(row.InboundTags, target.Inbound.Tag)
			}
			if target.Traffic != nil {
				row.Used += target.Traffic.Up + target.Traffic.Down
				if target.Traffic.LastOnline > lastOnline {
					lastOnline = target.Traffic.LastOnline
				}
			} else if ts := lastOnlineMap[target.Client.Email]; ts > lastOnline {
				lastOnline = ts
			}
			effectiveTotal := model.InitialClientTrafficQuota(target.Client)
			if target.Traffic != nil && target.Traffic.Total > 0 {
				effectiveTotal = target.Traffic.Total
			}
			if effectiveTotal > totalLimit {
				totalLimit = effectiveTotal
			}
			if target.Client.TotalGB > nominalLimit {
				nominalLimit = target.Client.TotalGB
				quotaFactor = 0
				if target.Client.QuotaFactor != nil {
					quotaFactor = *target.Client.QuotaFactor
				}
			}
			if target.Client.LimitIP > 0 && (limitIP == 0 || target.Client.LimitIP < limitIP) {
				limitIP = target.Client.LimitIP
			}
			if target.Client.ExpiryTime > 0 && (row.ExpiryTime == 0 || target.Client.ExpiryTime < row.ExpiryTime) {
				row.ExpiryTime = target.Client.ExpiryTime
			}
			if target.Client.Reset > row.Reset {
				row.Reset = target.Client.Reset
			}
			if !target.Client.Enable {
				row.Enabled = false
			}
			if _, ok := onlineSet[strings.ToLower(target.Client.Email)]; ok {
				row.Online = true
			}
		}
		row.ClientCount = len(row.InboundIDs)
		row.Total = totalLimit
		row.NominalTotal = nominalLimit
		row.QuotaFactor = quotaFactor
		row.LimitIP = limitIP
		row.LastOnline = lastOnline
		if row.Total > 0 {
			row.Remaining = row.Total - row.Used
			if row.Remaining < 0 {
				row.Remaining = 0
			}
			row.UsagePct = float64(row.Used) / float64(row.Total) * 100
			if row.UsagePct > 100 {
				row.UsagePct = 100
			}
		}
		row.Watching = row.Total > 0 && row.Used < row.Total && row.UsagePct >= 95
		row.Finished = (row.Total > 0 && row.Used >= row.Total) || (row.ExpiryTime > 0 && row.ExpiryTime <= now)
		expiringSoon := row.ExpiryTime > 0 && row.ExpiryTime > now && row.ExpiryTime-now <= (72*60*60*1000)
		switch {
		case row.Finished:
			row.Status = "finished"
			row.StatusLabel = "finished"
		case row.Online:
			row.Status = "online"
			row.StatusLabel = "online"
		case !row.Enabled:
			row.Status = "disabled"
			row.StatusLabel = "disabled"
		case expiringSoon:
			row.Status = "expiring"
			row.StatusLabel = "expiring soon"
		case row.Watching:
			row.Status = "watching"
			row.StatusLabel = "watching"
		default:
			row.Status = "active"
			row.StatusLabel = "active"
		}
		sort.Strings(row.Emails)
		sort.Ints(row.InboundIDs)
		sort.Strings(row.InboundTags)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SubID < rows[j].SubID })
	return rows, nil
}

func (s *MultiLinkService) List() ([]*MultiLinkRow, error) {
	return s.ListByUser(nil)
}

func (s *MultiLinkService) findTargetsByUser(subID string, userID *int) ([]multiLinkTarget, error) {
	grouped, err := s.collectTargets(userID)
	if err != nil {
		return nil, err
	}
	targets := grouped[subID]
	if len(targets) == 0 {
		return nil, common.NewError("multi link not found")
	}
	return targets, nil
}

func (s *MultiLinkService) SetEnabledByUser(subID string, enabled bool, userID *int) (bool, error) {
	targets, err := s.findTargetsByUser(subID, userID)
	if err != nil {
		return false, err
	}
	needRestart := false
	for _, target := range targets {
		client := target.Client
		client.Enable = enabled
		// The multilink toggle is a manual action: record it so no automatic
		// path (quota increase, expiry extension, traffic job) re-enables a
		// multilink the user turned off.
		if enabled {
			client.DisableReason = ""
		} else {
			client.DisableReason = DisableReasonManual
		}
		body, err := json.Marshal(map[string]any{"clients": []model.Client{client}})
		if err != nil {
			return needRestart, err
		}
		payload := &model.Inbound{Id: target.Inbound.Id, Settings: string(body)}
		restartOne, err := s.inboundService.UpdateInboundClient(payload, target.ClientID)
		if err != nil {
			return needRestart, err
		}
		needRestart = needRestart || restartOne
	}
	return needRestart, nil
}

func (s *MultiLinkService) SetEnabled(subID string, enabled bool) (bool, error) {
	return s.SetEnabledByUser(subID, enabled, nil)
}

func (s *MultiLinkService) UpdateByUser(subID string, update *MultiLinkUpdate, userID *int) (bool, error) {
	if update == nil {
		return false, common.NewError("empty update payload")
	}
	if update.TotalGB < 0 {
		return false, common.NewError("total traffic must be non-negative")
	}
	if update.LimitIP != nil && *update.LimitIP < 0 {
		return false, common.NewError("IP limit must be non-negative")
	}
	if !model.ValidateClientQuotaFactor(update.QuotaFactor) {
		return false, common.NewError(fmt.Sprintf("traffic coefficient must be between 0 and %d", model.MaxClientQuotaFactor))
	}
	currentRow, err := s.GetByUser(subID, userID)
	if err != nil {
		return false, err
	}
	targets, err := s.findTargetsByUser(subID, userID)
	if err != nil {
		return false, err
	}
	needRestart := false
	now := time.Now().UnixMilli()
	nextEffectiveQuota := MultiLinkQuotaAfterUpdate(currentRow, update)
	quotaRecovered := nextEffectiveQuota <= 0 || nextEffectiveQuota > currentRow.Used || update.ResetTraffic
	expiryRecovered := update.ExpiryTime <= 0 || update.ExpiryTime > now
	autoEnable := currentRow.Finished && quotaRecovered && expiryRecovered
	for _, target := range targets {
		client := target.Client
		client.TotalGB = update.TotalGB
		if update.QuotaFactor != nil {
			factor := *update.QuotaFactor
			client.QuotaFactor = &factor
		}
		if update.LimitIP != nil {
			client.LimitIP = *update.LimitIP
		}
		client.ExpiryTime = update.ExpiryTime
		client.Reset = update.Reset
		client.Enable = update.Enable || autoEnable
		body, err := json.Marshal(map[string]any{"clients": []model.Client{client}})
		if err != nil {
			return needRestart, err
		}
		payload := &model.Inbound{Id: target.Inbound.Id, Settings: string(body)}
		restartOne, err := s.inboundService.UpdateInboundClient(payload, target.ClientID)
		if err != nil {
			return needRestart, err
		}
		needRestart = needRestart || restartOne
		if update.ResetTraffic {
			restartTwo, err := s.inboundService.ResetClientTraffic(target.Inbound.Id, target.Client.Email)
			if err != nil {
				return needRestart, err
			}
			needRestart = needRestart || restartTwo
		}
	}
	return needRestart, nil
}

// MultiLinkQuotaAfterUpdate computes the shared effective allowance without
// mutating a client. Quota edits add only the scaled nominal delta; a reset
// starts a new cycle at the new nominal allowance divided by its factor.
func MultiLinkQuotaAfterUpdate(current *MultiLinkRow, update *MultiLinkUpdate) int64 {
	if current == nil || update == nil || update.TotalGB <= 0 {
		return 0
	}
	oldFactor := current.QuotaFactor
	oldClient := model.Client{TotalGB: current.NominalTotal}
	if oldFactor != 0 {
		oldClient.QuotaFactor = &oldFactor
	}
	newClient := model.Client{TotalGB: update.TotalGB}
	if update.QuotaFactor != nil {
		newClient.QuotaFactor = update.QuotaFactor
	} else if oldFactor != 0 {
		newClient.QuotaFactor = &oldFactor
	}
	if update.ResetTraffic {
		return model.InitialClientTrafficQuota(newClient)
	}
	if current.Total <= 0 && current.NominalTotal > 0 {
		return model.InitialClientTrafficQuota(newClient)
	}
	return model.UpdatedClientTrafficQuota(oldClient, newClient, current.Total)
}

func (s *MultiLinkService) Update(subID string, update *MultiLinkUpdate) (bool, error) {
	return s.UpdateByUser(subID, update, nil)
}

func (s *MultiLinkService) DeleteByUser(subID string, userID *int) (bool, error) {
	targets, err := s.findTargetsByUser(subID, userID)
	if err != nil {
		return false, err
	}
	needRestart := false
	for _, target := range targets {
		restartOne, err := s.inboundService.DelInboundClientByEmail(target.Inbound.Id, target.Client.Email)
		if err != nil {
			return needRestart, err
		}
		needRestart = needRestart || restartOne
	}
	return needRestart, nil
}

func (s *MultiLinkService) Delete(subID string) (bool, error) {
	return s.DeleteByUser(subID, nil)
}

func (s *MultiLinkService) GetByUser(subID string, userID *int) (*MultiLinkRow, error) {
	rows, err := s.ListByUser(userID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.SubID == subID {
			return row, nil
		}
	}
	return nil, common.NewError(fmt.Sprintf("multi link %s not found", subID))
}

func (s *MultiLinkService) Get(subID string) (*MultiLinkRow, error) {
	return s.GetByUser(subID, nil)
}
