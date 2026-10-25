package service

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/common"
	"github.com/mhsanaei/3x-ui/v2/util/crypto"
	ldaputil "github.com/mhsanaei/3x-ui/v2/util/ldap"
	"github.com/xlzd/gotp"
	"gorm.io/gorm"
)

// UserService provides business logic for user management and authentication.
// It handles user creation, login, password management, and 2FA operations.
type UserService struct {
	settingService SettingService
}

const (
	UserRoleAdmin    = "admin"
	UserRoleReseller = "reseller"
)

type ResellerPermissions struct {
	Overview   bool `json:"overview"`
	Inbounds   bool `json:"inbounds"`
	MultiLinks bool `json:"multiLinks"`
}

type ResellerView struct {
	Id                int                 `json:"id"`
	Username          string              `json:"username"`
	PortalPath        string              `json:"portalPath"`
	Enabled           bool                `json:"enabled"`
	MaxUsers          int                 `json:"maxUsers"`
	MaxTraffic        int64               `json:"maxTraffic"`
	Permissions       ResellerPermissions `json:"permissions"`
	AllowedInboundIDs []int               `json:"allowedInboundIds"`
	InboundCount      int                 `json:"inboundCount"`
	ClientCount       int                 `json:"clientCount"`
	AssignedTotal     int64               `json:"assignedTotal"`
	UsedTraffic       int64               `json:"usedTraffic"`
	RemainingUsers    int                 `json:"remainingUsers"`
	RemainingTraffic  int64               `json:"remainingTraffic"`
	CreatedAt         int64               `json:"createdAt"`
	UpdatedAt         int64               `json:"updatedAt"`
}

type ResellerUpdateOptions struct {
	ClientAction string  `json:"clientAction" form:"clientAction"`
	PortalPath   *string `json:"portalPath" form:"portalPath"`
}

type ResellerDeleteOptions struct {
	ClientAction string `json:"clientAction" form:"clientAction"`
}

func defaultResellerPermissions() ResellerPermissions {
	return ResellerPermissions{
		Overview:   true,
		Inbounds:   true,
		MultiLinks: true,
	}
}

func normalizeRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == UserRoleReseller {
		return UserRoleReseller
	}
	return UserRoleAdmin
}

func normalizeResellerPermissions(raw string) string {
	perms := defaultResellerPermissions()
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &perms)
	}
	body, _ := json.Marshal(perms)
	return string(body)
}

func parseResellerPermissions(raw string) ResellerPermissions {
	perms := defaultResellerPermissions()
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &perms)
	}
	return perms
}

func normalizeAllowedInboundIDs(ids []int) string {
	if ids == nil {
		return ""
	}
	seen := make(map[int]struct{}, len(ids))
	normalized := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	sort.Ints(normalized)
	body, _ := json.Marshal(normalized)
	return string(body)
}

func parseAllowedInboundIDs(raw string) []int {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var ids []int
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return []int{}
	}
	seen := make(map[int]struct{}, len(ids))
	normalized := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	sort.Ints(normalized)
	return normalized
}

func (s *UserService) resellerAllowedInboundSet(user *model.User) (map[int]struct{}, bool) {
	if user == nil || !s.IsReseller(user) {
		return nil, false
	}
	if strings.TrimSpace(user.AllowedInboundIDs) == "" {
		return nil, false
	}
	ids := parseAllowedInboundIDs(user.AllowedInboundIDs)
	allowed := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	return allowed, true
}

func (s *UserService) CanResellerUseInbound(user *model.User, inbound *model.Inbound) bool {
	if user == nil || inbound == nil {
		return false
	}
	if !s.IsReseller(user) {
		return true
	}
	if inbound.UserId == user.Id {
		return true
	}
	if user.ParentUserID == nil || inbound.UserId != *user.ParentUserID {
		return false
	}
	allowed, restricted := s.resellerAllowedInboundSet(user)
	if !restricted {
		return true
	}
	_, ok := allowed[inbound.Id]
	return ok
}

func (s *UserService) IsAdmin(user *model.User) bool {
	return user != nil && normalizeRole(user.Role) == UserRoleAdmin
}

func (s *UserService) IsReseller(user *model.User) bool {
	return user != nil && normalizeRole(user.Role) == UserRoleReseller
}

func (s *UserService) CanAccess(user *model.User, area string) bool {
	if user == nil {
		return false
	}
	if s.IsAdmin(user) {
		return true
	}
	perms := parseResellerPermissions(user.Permissions)
	switch area {
	case "overview":
		return perms.Overview
	case "inbounds":
		return perms.Inbounds
	case "multilinks":
		return perms.MultiLinks
	default:
		return false
	}
}

// FirstAllowedPanelArea selects the reseller's default landing area in a
// stable order so login redirects and panel routes use the same policy.
func (s *UserService) FirstAllowedPanelArea(user *model.User) string {
	for _, area := range []string{"overview", "inbounds", "multilinks"} {
		if s.CanAccess(user, area) {
			return area
		}
	}
	return ""
}

func HasAnyResellerPermission(perms ResellerPermissions) bool {
	return perms.Overview || perms.Inbounds || perms.MultiLinks
}

// GetFirstUser retrieves the first user from the database.
// This is typically used for initial setup or when there's only one admin user.
func (s *UserService) GetFirstUser() (*model.User, error) {
	db := database.GetDB()

	user := &model.User{}
	err := db.Model(model.User{}).
		First(user).
		Error
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (s *UserService) CheckUser(username string, password string, twoFactorCode string) (*model.User, error) {
	db := database.GetDB()

	user := &model.User{}

	err := db.Model(model.User{}).
		Where("username = ?", username).
		First(user).
		Error
	if err == gorm.ErrRecordNotFound {
		return nil, errors.New("invalid credentials")
	} else if err != nil {
		logger.Warning("check user err:", err)
		return nil, err
	}
	if !user.Enabled {
		return nil, errors.New("user is disabled")
	}
	user.Role = normalizeRole(user.Role)
	if s.IsReseller(user) && strings.TrimSpace(user.Permissions) == "" {
		user.Permissions = normalizeResellerPermissions(user.Permissions)
	}

	if !crypto.CheckPasswordHash(user.Password, password) {
		ldapEnabled, _ := s.settingService.GetLdapEnable()
		if !ldapEnabled {
			return nil, errors.New("invalid credentials")
		}

		host, _ := s.settingService.GetLdapHost()
		port, _ := s.settingService.GetLdapPort()
		useTLS, _ := s.settingService.GetLdapUseTLS()
		bindDN, _ := s.settingService.GetLdapBindDN()
		ldapPass, _ := s.settingService.GetLdapPassword()
		baseDN, _ := s.settingService.GetLdapBaseDN()
		userFilter, _ := s.settingService.GetLdapUserFilter()
		userAttr, _ := s.settingService.GetLdapUserAttr()

		cfg := ldaputil.Config{
			Host:       host,
			Port:       port,
			UseTLS:     useTLS,
			BindDN:     bindDN,
			Password:   ldapPass,
			BaseDN:     baseDN,
			UserFilter: userFilter,
			UserAttr:   userAttr,
		}
		ok, err := ldaputil.AuthenticateUser(cfg, username, password)
		if err != nil || !ok {
			return nil, errors.New("invalid credentials")
		}
	}

	twoFactorEnable, err := s.settingService.GetTwoFactorEnable()
	if err != nil {
		logger.Warning("check two factor err:", err)
		return nil, err
	}

	if twoFactorEnable {
		twoFactorToken, err := s.settingService.GetTwoFactorToken()

		if err != nil {
			logger.Warning("check two factor token err:", err)
			return nil, err
		}

		if gotp.NewDefaultTOTP(twoFactorToken).Now() != twoFactorCode {
			return nil, errors.New("invalid 2fa code")
		}
	}

	return user, nil
}

func (s *UserService) UpdateUser(id int, username string, password string) error {
	db := database.GetDB()
	hashedPassword, err := crypto.HashPasswordAsBcrypt(password)

	if err != nil {
		return err
	}

	twoFactorEnable, err := s.settingService.GetTwoFactorEnable()
	if err != nil {
		return err
	}

	if twoFactorEnable {
		s.settingService.SetTwoFactorEnable(false)
		s.settingService.SetTwoFactorToken("")
	}

	return db.Model(model.User{}).
		Where("id = ?", id).
		Updates(map[string]any{"username": username, "password": hashedPassword}).
		Error
}

func (s *UserService) UpdateFirstUser(username string, password string) error {
	if username == "" {
		return errors.New("username can not be empty")
	} else if password == "" {
		return errors.New("password can not be empty")
	}
	hashedPassword, er := crypto.HashPasswordAsBcrypt(password)

	if er != nil {
		return er
	}

	db := database.GetDB()
	user := &model.User{}
	err := db.Model(model.User{}).First(user).Error
	if database.IsNotFound(err) {
		user.Username = username
		user.Password = hashedPassword
		user.Role = UserRoleAdmin
		user.Enabled = true
		return db.Model(model.User{}).Create(user).Error
	} else if err != nil {
		return err
	}
	user.Username = username
	user.Password = hashedPassword
	user.Role = UserRoleAdmin
	user.Enabled = true
	return db.Save(user).Error
}

func (s *UserService) GetUserByID(id int) (*model.User, error) {
	user := &model.User{}
	if err := database.GetDB().Model(model.User{}).First(user, id).Error; err != nil {
		return nil, err
	}
	user.Role = normalizeRole(user.Role)
	return user, nil
}

func (s *UserService) GetUserByUsername(username string) (*model.User, error) {
	user := &model.User{}
	if err := database.GetDB().Model(model.User{}).Where("username = ?", strings.TrimSpace(username)).First(user).Error; err != nil {
		return nil, err
	}
	user.Role = normalizeRole(user.Role)
	return user, nil
}

func (s *UserService) GetResellerByUsername(username string) (*model.User, error) {
	user, err := s.GetUserByUsername(username)
	if err != nil {
		return nil, err
	}
	if !s.IsReseller(user) {
		return nil, common.NewError("reseller not found")
	}
	if err := s.ensureResellerPortalPath(user); err != nil {
		return nil, err
	}
	return user, nil
}

var resellerPortalPathPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func normalizeResellerPortalPath(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), "/")
	if value == "" {
		return "", nil
	}
	if !resellerPortalPathPattern.MatchString(value) {
		return "", common.NewError("reseller web path must be one URL-safe path segment")
	}
	switch strings.ToLower(value) {
	case "panel", "login", "logout", "gettwofactorenable", "ws":
		return "", common.NewError("reseller web path is reserved for a panel route")
	}
	return value, nil
}

func (s *UserService) uniqueResellerPortalPath(value string, excludeUserID int) (string, error) {
	value, err := normalizeResellerPortalPath(value)
	if err != nil {
		return "", err
	}
	mainBasePath, err := (&SettingService{}).GetBasePath()
	if err != nil {
		return "", err
	}
	mainPathSegment := strings.Trim(mainBasePath, "/")
	if value != "" && value == mainPathSegment {
		return "", common.NewError("reseller web path must be different from the main panel web path")
	}
	db := database.GetDB()
	if value == "" {
		return "", nil
	}
	var count int64
	if err := db.Model(&model.User{}).Where("role = ? AND portal_path = ? AND id <> ?", UserRoleReseller, value, excludeUserID).Count(&count).Error; err != nil {
		return "", err
	}
	if count > 0 {
		return "", common.NewError("reseller web path is already in use")
	}
	return value, nil
}

func (s *UserService) ensureResellerPortalPath(user *model.User) error {
	if user == nil || !s.IsReseller(user) {
		return nil
	}
	value, err := normalizeResellerPortalPath(user.PortalPath)
	if err != nil {
		return err
	}
	if value != user.PortalPath {
		if err := database.GetDB().Model(&model.User{}).Where("id = ? AND role = ?", user.Id, UserRoleReseller).Update("portal_path", value).Error; err != nil {
			return err
		}
	}
	user.PortalPath = value
	return nil
}

func resellerPortalPathAliases(raw string) []string {
	var aliases []string
	if err := json.Unmarshal([]byte(raw), &aliases); err != nil || aliases == nil {
		return []string{}
	}
	return aliases
}

func (s *UserService) IsResellerPortalPathAlias(user *model.User, path string) bool {
	if user == nil || strings.TrimSpace(path) == "" {
		return false
	}
	for _, alias := range resellerPortalPathAliases(user.PortalPathAliases) {
		if alias == path {
			return true
		}
	}
	return false
}

func (s *UserService) GetAccessibleInbounds(user *model.User) ([]*model.Inbound, error) {
	if user == nil {
		return []*model.Inbound{}, nil
	}
	if user.Id > 0 {
		freshUser, err := s.GetUserByID(user.Id)
		if err == nil && freshUser != nil {
			user = freshUser
		}
	}
	inboundService := &InboundService{}
	if !s.IsReseller(user) {
		return inboundService.GetInbounds(user.Id)
	}

	ownerIDs := []int{user.Id}
	if user.ParentUserID != nil && *user.ParentUserID > 0 && *user.ParentUserID != user.Id {
		ownerIDs = append(ownerIDs, *user.ParentUserID)
	} else {
		adminUser := &model.User{}
		if err := database.GetDB().Model(model.User{}).
			Where("role = ?", UserRoleAdmin).
			Order("id asc").
			First(adminUser).Error; err == nil && adminUser.Id > 0 && adminUser.Id != user.Id {
			ownerIDs = append(ownerIDs, adminUser.Id)
		}
	}

	seen := make(map[int]*model.Inbound)
	for _, ownerID := range ownerIDs {
		inbounds, err := inboundService.GetInbounds(ownerID)
		if err != nil {
			return nil, err
		}
		for _, inbound := range inbounds {
			if inbound == nil {
				continue
			}
			if !s.CanResellerUseInbound(user, inbound) {
				continue
			}
			seen[inbound.Id] = inbound
		}
	}

	rows := make([]*model.Inbound, 0, len(seen))
	for _, inbound := range seen {
		rows = append(rows, inbound)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Port == rows[j].Port {
			return rows[i].Id < rows[j].Id
		}
		return rows[i].Port < rows[j].Port
	})
	return rows, nil
}

func (s *UserService) GetUserQuotaSnapshot(userID int) (clientCount int, assignedTotal int64, usedTraffic int64, err error) {
	user, err := s.GetUserByID(userID)
	if err != nil {
		return 0, 0, 0, err
	}
	inbounds, err := s.GetAccessibleInbounds(user)
	if err != nil {
		return 0, 0, 0, err
	}
	if !s.IsReseller(user) {
		for _, inbound := range inbounds {
			clients, cerr := (&InboundService{}).GetClients(inbound)
			if cerr == nil {
				clientCount += len(clients)
				for _, client := range clients {
					if client.TotalGB > 0 {
						assignedTotal += client.TotalGB
					}
				}
			}
			usedTraffic += inbound.Up + inbound.Down
		}
		return clientCount, assignedTotal, usedTraffic, nil
	}

	type logicalQuota struct {
		Total int64
		Used  int64
	}
	logicalUsers := map[string]*logicalQuota{}
	for _, inbound := range inbounds {
		clients, cerr := (&InboundService{}).GetClients(inbound)
		ownedByEmail := map[string]string{}
		if cerr == nil {
			for _, client := range clients {
				if !ClientOwnedByReseller(client, user) {
					continue
				}
				key := strings.TrimSpace(client.SubID)
				if key == "" {
					key = "email:" + strings.ToLower(strings.TrimSpace(client.Email))
				} else {
					key = "sub:" + key
				}
				emailKey := strings.ToLower(strings.TrimSpace(client.Email))
				ownedByEmail[emailKey] = key
				entry, ok := logicalUsers[key]
				if !ok {
					entry = &logicalQuota{}
					logicalUsers[key] = entry
				}
				if client.TotalGB > entry.Total {
					entry.Total = client.TotalGB
				}
			}
		}
		for _, stat := range inbound.ClientStats {
			clientEmail := strings.ToLower(strings.TrimSpace(stat.Email))
			if clientEmail == "" {
				continue
			}
			key, ok := ownedByEmail[clientEmail]
			if !ok {
				continue
			}
			usage := stat.Up + stat.Down
			usedTraffic += usage
			entry := logicalUsers[key]
			if entry == nil {
				entry = &logicalQuota{}
				logicalUsers[key] = entry
			}
			entry.Used += usage
		}
	}
	for _, quota := range logicalUsers {
		clientCount++
		if quota == nil || quota.Total <= 0 {
			continue
		}
		remainingAssigned := quota.Total - quota.Used
		if remainingAssigned < 0 {
			remainingAssigned = 0
		}
		assignedTotal += remainingAssigned
	}
	return clientCount, assignedTotal, usedTraffic, nil
}

func (s *UserService) ListResellers(parentUserID int) ([]*ResellerView, error) {
	var users []*model.User
	if err := database.GetDB().Model(model.User{}).
		Where("role = ? AND parent_user_id = ?", UserRoleReseller, parentUserID).
		Order("id asc").
		Find(&users).Error; err != nil {
		return nil, err
	}
	rows := make([]*ResellerView, 0, len(users))
	for _, user := range users {
		if err := s.ensureResellerPortalPath(user); err != nil {
			return nil, err
		}
		clientCount, assignedTotal, usedTraffic, err := s.GetUserQuotaSnapshot(user.Id)
		if err != nil {
			return nil, err
		}
		inbounds, err := s.GetAccessibleInbounds(user)
		if err != nil {
			return nil, err
		}
		rows = append(rows, &ResellerView{
			Id:                user.Id,
			Username:          user.Username,
			PortalPath:        user.PortalPath,
			Enabled:           user.Enabled,
			MaxUsers:          user.MaxUsers,
			MaxTraffic:        user.MaxTraffic,
			Permissions:       parseResellerPermissions(user.Permissions),
			AllowedInboundIDs: parseAllowedInboundIDs(user.AllowedInboundIDs),
			InboundCount:      len(inbounds),
			ClientCount:       clientCount,
			AssignedTotal:     assignedTotal,
			UsedTraffic:       usedTraffic,
			RemainingUsers:    s.computeRemainingUsers(user.MaxUsers, clientCount),
			RemainingTraffic:  s.computeRemainingTraffic(user.MaxTraffic, assignedTotal),
			CreatedAt:         user.CreatedAt,
			UpdatedAt:         user.UpdatedAt,
		})
	}
	return rows, nil
}

func (s *UserService) GetResellerView(userID int) (*ResellerView, error) {
	user, err := s.GetUserByID(userID)
	if err != nil {
		return nil, err
	}
	if err := s.ensureResellerPortalPath(user); err != nil {
		return nil, err
	}
	clientCount, assignedTotal, usedTraffic, err := s.GetUserQuotaSnapshot(user.Id)
	if err != nil {
		return nil, err
	}
	inbounds, err := s.GetAccessibleInbounds(user)
	if err != nil {
		return nil, err
	}
	return &ResellerView{
		Id:                user.Id,
		Username:          user.Username,
		PortalPath:        user.PortalPath,
		Enabled:           user.Enabled,
		MaxUsers:          user.MaxUsers,
		MaxTraffic:        user.MaxTraffic,
		Permissions:       parseResellerPermissions(user.Permissions),
		AllowedInboundIDs: parseAllowedInboundIDs(user.AllowedInboundIDs),
		InboundCount:      len(inbounds),
		ClientCount:       clientCount,
		AssignedTotal:     assignedTotal,
		UsedTraffic:       usedTraffic,
		RemainingUsers:    s.computeRemainingUsers(user.MaxUsers, clientCount),
		RemainingTraffic:  s.computeRemainingTraffic(user.MaxTraffic, assignedTotal),
		CreatedAt:         user.CreatedAt,
		UpdatedAt:         user.UpdatedAt,
	}, nil
}

func (s *UserService) computeRemainingUsers(maxUsers, currentUsers int) int {
	if maxUsers <= 0 {
		return -1
	}
	if currentUsers >= maxUsers {
		return 0
	}
	return maxUsers - currentUsers
}

func (s *UserService) computeRemainingTraffic(maxTraffic, assignedTotal int64) int64 {
	if maxTraffic <= 0 {
		return -1
	}
	if assignedTotal >= maxTraffic {
		return 0
	}
	return maxTraffic - assignedTotal
}

func (s *UserService) sanitizeAllowedInboundIDs(parentUserID int, ids []int) ([]int, error) {
	if ids == nil {
		return nil, nil
	}
	normalized := parseAllowedInboundIDs(normalizeAllowedInboundIDs(ids))
	if len(normalized) == 0 {
		return []int{}, nil
	}
	inbounds, err := (&InboundService{}).GetInbounds(parentUserID)
	if err != nil {
		return nil, err
	}
	available := make(map[int]struct{}, len(inbounds))
	for _, inbound := range inbounds {
		if inbound == nil {
			continue
		}
		available[inbound.Id] = struct{}{}
	}
	for _, id := range normalized {
		if _, ok := available[id]; !ok {
			return nil, common.NewError("one or more allowed inbounds are invalid")
		}
	}
	return normalized, nil
}

func (s *UserService) CreateReseller(parentUserID int, username, password string, maxUsers int, maxTraffic int64, enabled bool, perms ResellerPermissions, allowedInboundIDs []int) (*model.User, error) {
	return s.CreateResellerWithPortalPath(parentUserID, username, password, "", maxUsers, maxTraffic, enabled, perms, allowedInboundIDs)
}

func (s *UserService) CreateResellerWithPortalPath(parentUserID int, username, password, portalPath string, maxUsers int, maxTraffic int64, enabled bool, perms ResellerPermissions, allowedInboundIDs []int) (*model.User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, common.NewError("username can not be empty")
	}
	if strings.TrimSpace(password) == "" {
		return nil, common.NewError("password can not be empty")
	}
	count := int64(0)
	if err := database.GetDB().Model(model.User{}).Where("username = ?", username).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, common.NewError("username already exists")
	}
	hashedPassword, err := crypto.HashPasswordAsBcrypt(password)
	if err != nil {
		return nil, err
	}
	allowedInboundIDs, err = s.sanitizeAllowedInboundIDs(parentUserID, allowedInboundIDs)
	if err != nil {
		return nil, err
	}
	portalPath, err = s.uniqueResellerPortalPath(portalPath, 0)
	if err != nil {
		return nil, err
	}
	user := &model.User{
		Username:          username,
		Password:          hashedPassword,
		Role:              UserRoleReseller,
		ParentUserID:      &parentUserID,
		Enabled:           enabled,
		MaxUsers:          maxUsers,
		MaxTraffic:        maxTraffic,
		PortalPath:        portalPath,
		Permissions:       normalizeResellerPermissions(mustJSON(perms)),
		AllowedInboundIDs: normalizeAllowedInboundIDs(allowedInboundIDs),
	}
	return user, database.GetDB().Create(user).Error
}

func (s *UserService) UpdateReseller(parentUserID, id int, username, password string, maxUsers int, maxTraffic int64, enabled bool, perms ResellerPermissions, allowedInboundIDs []int) (*model.User, error) {
	return s.UpdateResellerWithOptions(parentUserID, id, username, password, maxUsers, maxTraffic, enabled, perms, allowedInboundIDs, ResellerUpdateOptions{})
}

func (s *UserService) UpdateResellerWithOptions(parentUserID, id int, username, password string, maxUsers int, maxTraffic int64, enabled bool, perms ResellerPermissions, allowedInboundIDs []int, opts ResellerUpdateOptions) (*model.User, error) {
	user := &model.User{}
	if err := database.GetDB().Model(model.User{}).
		Where("id = ? AND role = ? AND parent_user_id = ?", id, UserRoleReseller, parentUserID).
		First(user).Error; err != nil {
		return nil, err
	}
	allowedInboundIDs, err := s.sanitizeAllowedInboundIDs(parentUserID, allowedInboundIDs)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{
		"username":            strings.TrimSpace(username),
		"enabled":             enabled,
		"max_users":           maxUsers,
		"max_traffic":         maxTraffic,
		"permissions":         normalizeResellerPermissions(mustJSON(perms)),
		"allowed_inbound_ids": normalizeAllowedInboundIDs(allowedInboundIDs),
	}
	if opts.PortalPath != nil {
		portalPath, err := s.uniqueResellerPortalPath(*opts.PortalPath, user.Id)
		if err != nil {
			return nil, err
		}
		updates["portal_path"] = portalPath
		if portalPath != user.PortalPath {
			aliases := resellerPortalPathAliases(user.PortalPathAliases)
			if oldPath, err := normalizeResellerPortalPath(user.PortalPath); err == nil && oldPath != "" {
				found := false
				for _, alias := range aliases {
					if alias == oldPath {
						found = true
						break
					}
				}
				if !found {
					aliases = append(aliases, oldPath)
				}
			}
			updates["portal_path_aliases"] = mustJSON(aliases)
		}
	}
	if strings.TrimSpace(password) != "" {
		hashedPassword, err := crypto.HashPasswordAsBcrypt(password)
		if err != nil {
			return nil, err
		}
		updates["password"] = hashedPassword
	}
	if err := s.backfillLegacyResellerOwnership(user); err != nil {
		return nil, err
	}
	if err := database.GetDB().Model(model.User{}).Where("id = ?", user.Id).Updates(updates).Error; err != nil {
		return nil, err
	}
	action := normalizeResellerLifecycleAction(opts.ClientAction)
	needRestart := false
	// An explicit "disable_clients" action records the disabled clients so the
	// reseller OFF -> ON cycle can restore exactly those clients later. Any
	// other action leaves the clients untouched here; the runtime sync below
	// is the single authority for quota-driven enable/disable.
	if user.Enabled && !enabled && action == ResellerLifecycleDisable {
		restart, err := s.DisableResellerOwnedClients(user.Id, true)
		if err != nil {
			return nil, err
		}
		needRestart = needRestart || restart
	}
	if err := s.SyncResellerRuntimeState(user.Id); err != nil {
		return nil, err
	}
	if needRestart {
		(&XrayService{}).SetToNeedRestart()
	}
	return s.GetUserByID(user.Id)
}

func (s *UserService) DeleteReseller(parentUserID, id int) error {
	return s.DeleteResellerWithOptions(parentUserID, id, ResellerDeleteOptions{})
}

func (s *UserService) DeleteResellerWithOptions(parentUserID, id int, opts ResellerDeleteOptions) error {
	user := &model.User{}
	if err := database.GetDB().Model(model.User{}).
		Where("id = ? AND role = ? AND parent_user_id = ?", id, UserRoleReseller, parentUserID).
		First(user).Error; err != nil {
		return err
	}
	action := normalizeResellerLifecycleAction(opts.ClientAction)
	needRestart := false
	switch action {
	case ResellerLifecycleDelete:
		restart, err := s.DeleteResellerOwnedClients(user.Id)
		if err != nil {
			return err
		}
		needRestart = needRestart || restart
	case ResellerLifecycleDisable:
		restart, err := s.DisableResellerOwnedClients(user.Id, false)
		if err != nil {
			return err
		}
		needRestart = needRestart || restart
	}
	if err := database.GetDB().Delete(&model.User{}, user.Id).Error; err != nil {
		return err
	}
	if needRestart {
		(&XrayService{}).SetToNeedRestart()
	}
	return nil
}

func mustJSON(v any) string {
	body, _ := json.Marshal(v)
	return string(body)
}

func (s *UserService) SyncResellerRuntimeState(userID int) error {
	user, err := s.GetUserByID(userID)
	if err != nil {
		return err
	}
	if !s.IsReseller(user) {
		return nil
	}
	_, _, usedTraffic, err := s.GetUserQuotaSnapshot(user.Id)
	if err != nil {
		return err
	}
	overQuota := user.MaxTraffic > 0 && usedTraffic >= user.MaxTraffic
	if overQuota {
		// Quota exhaustion disables and records owned clients so they can be
		// restored automatically once the quota is raised again.
		changed, err := s.DisableResellerOwnedClients(user.Id, true)
		if err != nil {
			return err
		}
		if changed {
			(&XrayService{}).SetToNeedRestart()
		}
		return nil
	}
	if user.Enabled {
		changed, err := s.RestoreManagedResellerClients(user.Id)
		if err != nil {
			return err
		}
		if changed {
			(&XrayService{}).SetToNeedRestart()
		}
		return nil
	}
	// Reseller is disabled and under quota: clients keep their current state
	// (lifecycle action "none" must not touch the reseller's clients).
	return nil
}

func (s *UserService) SyncAllResellerRuntimeStates() error {
	var users []*model.User
	if err := database.GetDB().Model(model.User{}).Where("role = ?", UserRoleReseller).Find(&users).Error; err != nil {
		return err
	}
	for _, user := range users {
		if err := s.SyncResellerRuntimeState(user.Id); err != nil {
			return err
		}
	}
	return nil
}
