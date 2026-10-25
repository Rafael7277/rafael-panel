package service

import (
	"database/sql"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/common"
	"github.com/mhsanaei/3x-ui/v2/util/crypto"
	"github.com/mhsanaei/3x-ui/v2/xray"
	"gorm.io/gorm"
	_ "modernc.org/sqlite"
)

type DBImportPreview struct {
	DetectedFormat     string `json:"detectedFormat"`
	WebDomain          string `json:"webDomain"`
	WebBasePath        string `json:"webBasePath"`
	WebPort            int    `json:"webPort"`
	CurrentWebDomain   string `json:"currentWebDomain"`
	CurrentWebBasePath string `json:"currentWebBasePath"`
	CurrentWebPort     int    `json:"currentWebPort"`
	CurrentHost        string `json:"currentHost"`
}

type DBImportOptions struct {
	PreserveCurrentAccess bool `json:"preserveCurrentAccess" form:"preserveCurrentAccess"`
}

type DBImportResult struct {
	RedirectURL string `json:"redirectUrl"`
}

type legacySetting struct {
	Id    int
	Key   string
	Value string
}

func (legacySetting) TableName() string { return "settings" }

type legacyUser struct {
	Id                int
	Username          string
	Password          string
	Role              string
	ParentUserID      int
	Enabled           bool
	MaxUsers          int
	MaxTraffic        int64
	Permissions       string
	AllowedInboundIDs string
	ManagedState      string
	CreatedAt         int64
	UpdatedAt         int64
}

func (legacyUser) TableName() string { return "users" }

type legacyInbound struct {
	Id                   int
	UserId               int
	Up                   int64
	Down                 int64
	Total                int64
	AllTime              int64
	Remark               string
	Enable               bool
	ExpiryTime           int64
	TrafficReset         string
	LastTrafficResetTime int64
	Listen               string
	Port                 int
	Protocol             string
	Settings             string
	StreamSettings       string
	Tag                  string
	Sniffing             string
	NodeID               int
	NodeInboundID        int
}

func (legacyInbound) TableName() string { return "inbounds" }

type legacyClientTraffic struct {
	Id         int
	InboundId  int
	Enable     bool
	Email      string
	Up         int64
	Down       int64
	AllTime    int64
	ExpiryTime int64
	Total      int64
	Reset      int
	LastOnline int64
}

func (legacyClientTraffic) TableName() string { return "client_traffics" }

type legacyOutboundTraffic struct {
	Id    int
	Tag   string
	Up    int64
	Down  int64
	Total int64
}

func (legacyOutboundTraffic) TableName() string { return "outbound_traffics" }

type legacyInboundClientIP struct {
	Id          int
	ClientEmail string
	Ips         string
}

func (legacyInboundClientIP) TableName() string { return "inbound_client_ips" }

type legacyHistorySeeder struct {
	Id         int
	SeederName string
}

func (legacyHistorySeeder) TableName() string { return "history_of_seeders" }

type legacyCustomGeoResource struct {
	Id            int
	Type          string `gorm:"column:geo_type"`
	Alias         string
	Url           string
	LocalPath     string `gorm:"column:local_path"`
	LastUpdatedAt int64  `gorm:"column:last_updated_at"`
	LastModified  string `gorm:"column:last_modified"`
	CreatedAt     int64  `gorm:"column:created_at"`
	UpdatedAt     int64  `gorm:"column:updated_at"`
}

func (legacyCustomGeoResource) TableName() string { return "custom_geo_resources" }

type legacyBackupData struct {
	Settings           []legacySetting
	Users              []legacyUser
	Inbounds           []legacyInbound
	ClientTraffics     []legacyClientTraffic
	OutboundTraffics   []legacyOutboundTraffic
	InboundClientIPs   []legacyInboundClientIP
	HistoryOfSeeders   []legacyHistorySeeder
	ApiTokens          []model.ApiToken
	CustomGeoResources []legacyCustomGeoResource
	Nodes              []model.Node
	NodeClientTraffics []model.NodeClientTraffic
}

func saveUploadedSQLiteTemp(file multipart.File) (string, func(), error) {
	tempDir, err := os.MkdirTemp("", "rafael-db-import-*")
	if err != nil {
		return "", nil, err
	}
	tempPath := filepath.Join(tempDir, "import.db")
	out, err := os.Create(tempPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return "", nil, err
	}
	if _, err = io.Copy(out, file); err != nil {
		_ = out.Close()
		_ = os.RemoveAll(tempDir)
		return "", nil, err
	}
	if err = out.Close(); err != nil {
		_ = os.RemoveAll(tempDir)
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	return tempPath, cleanup, nil
}

func openLegacySQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func loadLegacyBackupData(path string) (*legacyBackupData, error) {
	src, err := openLegacySQLite(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	data := &legacyBackupData{}
	if data.Settings, err = loadLegacySettings(src); err != nil {
		return nil, err
	}
	if data.Users, err = loadLegacyUsers(src); err != nil {
		return nil, err
	}
	if data.Inbounds, err = loadLegacyInbounds(src); err != nil {
		return nil, err
	}
	data.ClientTraffics, _ = loadLegacyClientTraffics(src)
	data.OutboundTraffics, _ = loadLegacyOutboundTraffics(src)
	data.InboundClientIPs, _ = loadLegacyInboundClientIPs(src)
	data.HistoryOfSeeders, _ = loadLegacyHistoryOfSeeders(src)
	data.ApiTokens, _ = loadLegacyApiTokens(src)
	if hasTable, err := legacySQLiteTableExists(src, "custom_geo_resources"); err == nil && hasTable {
		data.CustomGeoResources, _ = loadLegacyCustomGeoResources(src)
	}
	data.Nodes, _ = loadLegacyNodes(src)
	data.NodeClientTraffics, _ = loadLegacyNodeClientTraffics(src)
	return data, nil
}

func legacySQLiteTableExists(db *sql.DB, table string) (bool, error) {
	row := db.QueryRow("SELECT COUNT(1) FROM sqlite_master WHERE type = 'table' AND name = ?", table)
	var count int
	if err := row.Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func legacySQLiteColumnExists(db *sql.DB, table string, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func legacySQLiteSelectExpr(db *sql.DB, table string, column string, fallback string) string {
	exists, err := legacySQLiteColumnExists(db, table, column)
	if err != nil || !exists {
		return fallback
	}
	return fmt.Sprintf("COALESCE(%s, %s)", column, fallback)
}

func loadLegacySettings(db *sql.DB) ([]legacySetting, error) {
	rows, err := db.Query("SELECT id, COALESCE(key, ''), COALESCE(value, '') FROM settings ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacySetting{}
	for rows.Next() {
		var item legacySetting
		if err := rows.Scan(&item.Id, &item.Key, &item.Value); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyUsers(db *sql.DB) ([]legacyUser, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT
		id,
		COALESCE(username, ''),
		COALESCE(password, ''),
		%s,
		%s,
		%s,
		%s,
		%s,
		%s,
		%s,
		%s,
		%s,
		%s
	FROM users ORDER BY id`,
		legacySQLiteSelectExpr(db, "users", "role", "'admin'"),
		legacySQLiteSelectExpr(db, "users", "parent_user_id", "0"),
		legacySQLiteSelectExpr(db, "users", "enabled", "1"),
		legacySQLiteSelectExpr(db, "users", "max_users", "0"),
		legacySQLiteSelectExpr(db, "users", "max_traffic", "0"),
		legacySQLiteSelectExpr(db, "users", "permissions", "''"),
		legacySQLiteSelectExpr(db, "users", "allowed_inbound_ids", "''"),
		legacySQLiteSelectExpr(db, "users", "managed_state", "''"),
		legacySQLiteSelectExpr(db, "users", "created_at", "0"),
		legacySQLiteSelectExpr(db, "users", "updated_at", "0"),
	))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacyUser{}
	for rows.Next() {
		var item legacyUser
		var enabled int64
		if err := rows.Scan(&item.Id, &item.Username, &item.Password, &item.Role, &item.ParentUserID, &enabled, &item.MaxUsers, &item.MaxTraffic, &item.Permissions, &item.AllowedInboundIDs, &item.ManagedState, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyInbounds(db *sql.DB) ([]legacyInbound, error) {
	query := fmt.Sprintf(`SELECT
		id,
		COALESCE(user_id, 0),
		COALESCE(up, 0),
		COALESCE(down, 0),
		COALESCE(total, 0),
		COALESCE(all_time, 0),
		COALESCE(remark, ''),
		COALESCE(enable, 0),
		COALESCE(expiry_time, 0),
		COALESCE(traffic_reset, ''),
		COALESCE(last_traffic_reset_time, 0),
		COALESCE(listen, ''),
		COALESCE(port, 0),
		COALESCE(protocol, ''),
		COALESCE(settings, ''),
		COALESCE(stream_settings, ''),
		COALESCE(tag, ''),
		COALESCE(sniffing, ''),
		%s,
		%s
	FROM inbounds ORDER BY port, id`,
		legacySQLiteSelectExpr(db, "inbounds", "node_id", "0"),
		legacySQLiteSelectExpr(db, "inbounds", "node_inbound_id", "0"),
	)
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacyInbound{}
	for rows.Next() {
		var item legacyInbound
		var enabled int64
		if err := rows.Scan(
			&item.Id,
			&item.UserId,
			&item.Up,
			&item.Down,
			&item.Total,
			&item.AllTime,
			&item.Remark,
			&enabled,
			&item.ExpiryTime,
			&item.TrafficReset,
			&item.LastTrafficResetTime,
			&item.Listen,
			&item.Port,
			&item.Protocol,
			&item.Settings,
			&item.StreamSettings,
			&item.Tag,
			&item.Sniffing,
			&item.NodeID,
			&item.NodeInboundID,
		); err != nil {
			return nil, err
		}
		item.Enable = enabled != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyApiTokens(db *sql.DB) ([]model.ApiToken, error) {
	hasTable, err := legacySQLiteTableExists(db, "api_tokens")
	if err != nil || !hasTable {
		return []model.ApiToken{}, err
	}
	rows, err := db.Query(`SELECT
		id,
		COALESCE(name, ''),
		COALESCE(token, ''),
		COALESCE(enabled, 1),
		COALESCE(created_at, 0)
	FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.ApiToken{}
	for rows.Next() {
		var item model.ApiToken
		var enabled int64
		if err := rows.Scan(&item.Id, &item.Name, &item.Token, &enabled, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyClientTraffics(db *sql.DB) ([]legacyClientTraffic, error) {
	hasTable, err := legacySQLiteTableExists(db, "client_traffics")
	if err != nil || !hasTable {
		return []legacyClientTraffic{}, err
	}
	rows, err := db.Query(`SELECT
		id,
		COALESCE(inbound_id, 0),
		COALESCE(enable, 0),
		COALESCE(email, ''),
		COALESCE(up, 0),
		COALESCE(down, 0),
		COALESCE(all_time, 0),
		COALESCE(expiry_time, 0),
		COALESCE(total, 0),
		COALESCE(reset, 0),
		COALESCE(last_online, 0)
	FROM client_traffics ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacyClientTraffic{}
	for rows.Next() {
		var item legacyClientTraffic
		var enabled int64
		if err := rows.Scan(
			&item.Id,
			&item.InboundId,
			&enabled,
			&item.Email,
			&item.Up,
			&item.Down,
			&item.AllTime,
			&item.ExpiryTime,
			&item.Total,
			&item.Reset,
			&item.LastOnline,
		); err != nil {
			return nil, err
		}
		item.Enable = enabled != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyOutboundTraffics(db *sql.DB) ([]legacyOutboundTraffic, error) {
	hasTable, err := legacySQLiteTableExists(db, "outbound_traffics")
	if err != nil || !hasTable {
		return []legacyOutboundTraffic{}, err
	}
	rows, err := db.Query("SELECT id, COALESCE(tag, ''), COALESCE(up, 0), COALESCE(down, 0), COALESCE(total, 0) FROM outbound_traffics ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacyOutboundTraffic{}
	for rows.Next() {
		var item legacyOutboundTraffic
		if err := rows.Scan(&item.Id, &item.Tag, &item.Up, &item.Down, &item.Total); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyInboundClientIPs(db *sql.DB) ([]legacyInboundClientIP, error) {
	hasTable, err := legacySQLiteTableExists(db, "inbound_client_ips")
	if err != nil || !hasTable {
		return []legacyInboundClientIP{}, err
	}
	rows, err := db.Query("SELECT id, COALESCE(client_email, ''), COALESCE(ips, '') FROM inbound_client_ips ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacyInboundClientIP{}
	for rows.Next() {
		var item legacyInboundClientIP
		if err := rows.Scan(&item.Id, &item.ClientEmail, &item.Ips); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyHistoryOfSeeders(db *sql.DB) ([]legacyHistorySeeder, error) {
	hasTable, err := legacySQLiteTableExists(db, "history_of_seeders")
	if err != nil || !hasTable {
		return []legacyHistorySeeder{}, err
	}
	rows, err := db.Query("SELECT id, COALESCE(seeder_name, '') FROM history_of_seeders ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacyHistorySeeder{}
	for rows.Next() {
		var item legacyHistorySeeder
		if err := rows.Scan(&item.Id, &item.SeederName); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyCustomGeoResources(db *sql.DB) ([]legacyCustomGeoResource, error) {
	rows, err := db.Query(`SELECT
		id,
		COALESCE(geo_type, ''),
		COALESCE(alias, ''),
		COALESCE(url, ''),
		COALESCE(local_path, ''),
		COALESCE(last_updated_at, 0),
		COALESCE(last_modified, ''),
		COALESCE(created_at, 0),
		COALESCE(updated_at, 0)
	FROM custom_geo_resources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []legacyCustomGeoResource{}
	for rows.Next() {
		var item legacyCustomGeoResource
		if err := rows.Scan(
			&item.Id,
			&item.Type,
			&item.Alias,
			&item.Url,
			&item.LocalPath,
			&item.LastUpdatedAt,
			&item.LastModified,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyNodes(db *sql.DB) ([]model.Node, error) {
	hasTable, err := legacySQLiteTableExists(db, "nodes")
	if err != nil || !hasTable {
		return []model.Node{}, err
	}
	rows, err := db.Query(`SELECT
		id,
		COALESCE(name, ''),
		COALESCE(remark, ''),
		COALESCE(scheme, ''),
		COALESCE(address, ''),
		COALESCE(port, 0),
		COALESCE(base_path, ''),
		COALESCE(api_token, ''),
		COALESCE(enable, 1),
		COALESCE(status, ''),
		COALESCE(last_heartbeat, 0),
		COALESCE(latency_ms, 0),
		COALESCE(xray_version, ''),
		COALESCE(panel_version, ''),
		COALESCE(last_error, ''),
		COALESCE(created_at, 0),
		COALESCE(updated_at, 0)
	FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.Node{}
	for rows.Next() {
		var item model.Node
		var enabled int64
		if err := rows.Scan(&item.Id, &item.Name, &item.Remark, &item.Scheme, &item.Address, &item.Port, &item.BasePath, &item.ApiToken, &enabled, &item.Status, &item.LastHeartbeat, &item.LatencyMs, &item.XrayVersion, &item.PanelVersion, &item.LastError, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Enable = enabled != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func loadLegacyNodeClientTraffics(db *sql.DB) ([]model.NodeClientTraffic, error) {
	hasTable, err := legacySQLiteTableExists(db, "node_client_traffics")
	if err != nil || !hasTable {
		return []model.NodeClientTraffic{}, err
	}
	rows, err := db.Query(`SELECT
		id,
		COALESCE(node_id, 0),
		COALESCE(email, ''),
		COALESCE(up, 0),
		COALESCE(down, 0)
	FROM node_client_traffics ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.NodeClientTraffic{}
	for rows.Next() {
		var item model.NodeClientTraffic
		if err := rows.Scan(&item.Id, &item.NodeId, &item.Email, &item.Up, &item.Down); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func legacySettingsMap(settings []legacySetting) map[string]string {
	result := make(map[string]string, len(settings))
	for _, item := range settings {
		result[item.Key] = item.Value
	}
	return result
}

func normalizeImportBasePath(raw string) string {
	basePath := strings.TrimSpace(raw)
	if basePath == "" {
		return "/"
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}
	return basePath
}

func parseIntSetting(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}

func splitHostPortFallback(rawHost string) (string, string) {
	host := strings.TrimSpace(rawHost)
	if host == "" {
		return "", ""
	}
	if parsedHost, parsedPort, err := net.SplitHostPort(host); err == nil {
		return parsedHost, parsedPort
	}
	if strings.Count(host, ":") == 1 && !strings.HasPrefix(host, "[") {
		parts := strings.SplitN(host, ":", 2)
		return parts[0], parts[1]
	}
	return host, ""
}

func buildImportRedirectURL(currentScheme, currentHost string, settings map[string]string) string {
	scheme := strings.TrimSpace(currentScheme)
	if scheme == "" {
		scheme = "http"
	}
	hostOnly, currentPort := splitHostPortFallback(currentHost)
	if hostOnly == "" {
		hostOnly = currentHost
	}
	targetHost := strings.TrimSpace(settings["webDomain"])
	if targetHost == "" {
		targetHost = hostOnly
	}
	targetPort := strings.TrimSpace(settings["webPort"])
	if targetPort == "" {
		targetPort = currentPort
	}
	basePath := normalizeImportBasePath(settings["webBasePath"])
	if targetHost == "" {
		return basePath + "panel/"
	}
	if targetPort == "" {
		return fmt.Sprintf("%s://%s%spanel/", scheme, targetHost, basePath)
	}
	return fmt.Sprintf("%s://%s:%s%spanel/", scheme, targetHost, targetPort, basePath)
}

func normalizeImportedPassword(raw string) (string, error) {
	password := strings.TrimSpace(raw)
	if password == "" {
		return crypto.HashPasswordAsBcrypt("admin")
	}
	if strings.HasPrefix(password, "$2a$") || strings.HasPrefix(password, "$2b$") || strings.HasPrefix(password, "$2y$") {
		return password, nil
	}
	return crypto.HashPasswordAsBcrypt(password)
}

func (s *ServerService) PreviewDBImport(file multipart.File, currentHost string) (*DBImportPreview, error) {
	tempPath, cleanup, err := saveUploadedSQLiteTemp(file)
	if err != nil {
		return nil, common.NewErrorf("error saving db: %v", err)
	}
	defer cleanup()
	if err = database.ValidateSQLiteDB(tempPath); err != nil {
		return nil, common.NewErrorf("invalid or corrupt db file: %v", err)
	}
	data, err := loadLegacyBackupData(tempPath)
	if err != nil {
		return nil, common.NewErrorf("error reading db: %v", err)
	}
	settings := legacySettingsMap(data.Settings)
	currentDomain, _ := (&SettingService{}).GetWebDomain()
	currentBasePath, _ := (&SettingService{}).GetBasePath()
	currentPort, _ := (&SettingService{}).GetPort()
	return &DBImportPreview{
		DetectedFormat:     "sqlite-compatible",
		WebDomain:          strings.TrimSpace(settings["webDomain"]),
		WebBasePath:        normalizeImportBasePath(settings["webBasePath"]),
		WebPort:            parseIntSetting(settings["webPort"]),
		CurrentWebDomain:   strings.TrimSpace(currentDomain),
		CurrentWebBasePath: normalizeImportBasePath(currentBasePath),
		CurrentWebPort:     currentPort,
		CurrentHost:        currentHost,
	}, nil
}

func clearCurrentDatabase(tx *gorm.DB) error {
	models := []any{
		&model.NodeClientTraffic{},
		&xray.ClientTraffic{},
		&model.InboundClientIps{},
		&model.OutboundTraffics{},
		&model.Inbound{},
		&model.CustomGeoResource{},
		&model.ApiToken{},
		&model.Node{},
		&model.Setting{},
		&model.User{},
		&model.HistoryOfSeeders{},
	}
	for _, item := range models {
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(item).Error; err != nil {
			return err
		}
	}
	return nil
}

func syncPostgresSequence(tx *gorm.DB, table string) error {
	if tx.Dialector.Name() != database.DialectPostgres {
		return nil
	}
	query := fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', 'id'), COALESCE((SELECT MAX(id) FROM %s), 1), COALESCE((SELECT MAX(id) FROM %s), 0) > 0)", table, table, table)
	return tx.Exec(query).Error
}

func importLegacyBackupData(tx *gorm.DB, data *legacyBackupData, preserveCurrentAccess bool, currentSettings map[string]string) error {
	if err := clearCurrentDatabase(tx); err != nil {
		return err
	}

	settings := legacySettingsMap(data.Settings)
	if preserveCurrentAccess {
		for _, key := range []string{"webListen", "webDomain", "webPort", "webCertFile", "webKeyFile", "webBasePath"} {
			if value, ok := currentSettings[key]; ok {
				settings[key] = value
			}
		}
	}

	importedUsers := make([]model.User, 0, len(data.Users))
	primaryAdminID := 1
	for _, row := range data.Users {
		password, err := normalizeImportedPassword(row.Password)
		if err != nil {
			return err
		}
		role := strings.TrimSpace(row.Role)
		if role == "" {
			role = UserRoleAdmin
		}
		var parentUserID *int
		if row.ParentUserID > 0 {
			parentID := row.ParentUserID
			parentUserID = &parentID
		}
		user := model.User{
			Id:                row.Id,
			Username:          strings.TrimSpace(row.Username),
			Password:          password,
			Role:              role,
			ParentUserID:      parentUserID,
			Enabled:           row.Enabled,
			MaxUsers:          row.MaxUsers,
			MaxTraffic:        row.MaxTraffic,
			Permissions:       row.Permissions,
			AllowedInboundIDs: row.AllowedInboundIDs,
			ManagedState:      row.ManagedState,
			CreatedAt:         row.CreatedAt,
			UpdatedAt:         row.UpdatedAt,
		}
		if user.Username == "" {
			continue
		}
		if role == UserRoleAdmin && (primaryAdminID == 1 || user.Id < primaryAdminID) {
			primaryAdminID = user.Id
		}
		importedUsers = append(importedUsers, user)
	}
	if len(importedUsers) == 0 {
		password, err := crypto.HashPasswordAsBcrypt("admin")
		if err != nil {
			return err
		}
		importedUsers = append(importedUsers, model.User{
			Id:       1,
			Username: "admin",
			Password: password,
			Role:     UserRoleAdmin,
			Enabled:  true,
		})
		primaryAdminID = 1
	}
	if err := tx.Create(&importedUsers).Error; err != nil {
		return err
	}

	importedSettings := make([]model.Setting, 0, len(settings))
	nextSettingID := 1
	for key, value := range settings {
		importedSettings = append(importedSettings, model.Setting{
			Id:    nextSettingID,
			Key:   key,
			Value: value,
		})
		nextSettingID++
	}
	if len(importedSettings) > 0 {
		if err := tx.Create(&importedSettings).Error; err != nil {
			return err
		}
	}

	importedInbounds := make([]model.Inbound, 0, len(data.Inbounds))
	for _, row := range data.Inbounds {
		userID := row.UserId
		if userID <= 0 {
			userID = primaryAdminID
		}
		importedInbounds = append(importedInbounds, model.Inbound{
			Id:                   row.Id,
			UserId:               userID,
			Up:                   row.Up,
			Down:                 row.Down,
			Total:                row.Total,
			AllTime:              row.AllTime,
			Remark:               row.Remark,
			Enable:               row.Enable,
			ExpiryTime:           row.ExpiryTime,
			TrafficReset:         row.TrafficReset,
			LastTrafficResetTime: row.LastTrafficResetTime,
			Listen:               row.Listen,
			Port:                 row.Port,
			Protocol:             model.Protocol(row.Protocol),
			Settings:             row.Settings,
			StreamSettings:       row.StreamSettings,
			Tag:                  row.Tag,
			Sniffing:             row.Sniffing,
			NodeID:               nullablePositiveInt(row.NodeID),
			NodeInboundID:        nullablePositiveInt(row.NodeInboundID),
		})
	}
	if len(importedInbounds) > 0 {
		if err := tx.Create(&importedInbounds).Error; err != nil {
			return err
		}
	}

	importedTraffics := make([]xray.ClientTraffic, 0, len(data.ClientTraffics))
	for _, row := range data.ClientTraffics {
		importedTraffics = append(importedTraffics, xray.ClientTraffic{
			Id:         row.Id,
			InboundId:  row.InboundId,
			Enable:     row.Enable,
			Email:      row.Email,
			Up:         row.Up,
			Down:       row.Down,
			AllTime:    row.AllTime,
			ExpiryTime: row.ExpiryTime,
			Total:      row.Total,
			Reset:      row.Reset,
			LastOnline: row.LastOnline,
		})
	}
	if len(importedTraffics) > 0 {
		if err := tx.Create(&importedTraffics).Error; err != nil {
			return err
		}
	}

	importedOutbounds := make([]model.OutboundTraffics, 0, len(data.OutboundTraffics))
	for _, row := range data.OutboundTraffics {
		importedOutbounds = append(importedOutbounds, model.OutboundTraffics{
			Id:    row.Id,
			Tag:   row.Tag,
			Up:    row.Up,
			Down:  row.Down,
			Total: row.Total,
		})
	}
	if len(importedOutbounds) > 0 {
		if err := tx.Create(&importedOutbounds).Error; err != nil {
			return err
		}
	}

	importedIPs := make([]model.InboundClientIps, 0, len(data.InboundClientIPs))
	for _, row := range data.InboundClientIPs {
		importedIPs = append(importedIPs, model.InboundClientIps{
			Id:          row.Id,
			ClientEmail: row.ClientEmail,
			Ips:         row.Ips,
		})
	}
	if len(importedIPs) > 0 {
		if err := tx.Create(&importedIPs).Error; err != nil {
			return err
		}
	}

	importedSeeders := make([]model.HistoryOfSeeders, 0, len(data.HistoryOfSeeders))
	for _, row := range data.HistoryOfSeeders {
		importedSeeders = append(importedSeeders, model.HistoryOfSeeders{
			Id:         row.Id,
			SeederName: row.SeederName,
		})
	}
	if len(importedSeeders) > 0 {
		if err := tx.Create(&importedSeeders).Error; err != nil {
			return err
		}
	}

	if len(data.ApiTokens) > 0 {
		if err := tx.Create(&data.ApiTokens).Error; err != nil {
			return err
		}
	}

	if len(data.CustomGeoResources) > 0 {
		importedResources := make([]model.CustomGeoResource, 0, len(data.CustomGeoResources))
		for _, row := range data.CustomGeoResources {
			importedResources = append(importedResources, model.CustomGeoResource{
				Id:            row.Id,
				Type:          row.Type,
				Alias:         row.Alias,
				Url:           row.Url,
				LocalPath:     row.LocalPath,
				LastUpdatedAt: row.LastUpdatedAt,
				LastModified:  row.LastModified,
				CreatedAt:     row.CreatedAt,
				UpdatedAt:     row.UpdatedAt,
			})
		}
		if err := tx.Create(&importedResources).Error; err != nil {
			return err
		}
	}

	if len(data.Nodes) > 0 {
		if err := tx.Create(&data.Nodes).Error; err != nil {
			return err
		}
	}

	if len(data.NodeClientTraffics) > 0 {
		if err := tx.Create(&data.NodeClientTraffics).Error; err != nil {
			return err
		}
	}

	for _, table := range []string{
		"users",
		"settings",
		"inbounds",
		"client_traffics",
		"outbound_traffics",
		"inbound_client_ips",
		"history_of_seeders",
		"api_tokens",
		"custom_geo_resources",
		"nodes",
		"node_client_traffics",
	} {
		if err := syncPostgresSequence(tx, table); err != nil {
			return err
		}
	}

	return nil
}

func nullablePositiveInt(v int) *int {
	if v <= 0 {
		return nil
	}
	out := v
	return &out
}

func (s *ServerService) ImportCompatibleDB(file multipart.File, opts DBImportOptions, currentScheme, currentHost string) (*DBImportResult, error) {
	tempPath, cleanup, err := saveUploadedSQLiteTemp(file)
	if err != nil {
		return nil, common.NewErrorf("error saving db: %v", err)
	}
	defer cleanup()
	if err = database.ValidateSQLiteDB(tempPath); err != nil {
		return nil, common.NewErrorf("invalid or corrupt db file: %v", err)
	}
	data, err := loadLegacyBackupData(tempPath)
	if err != nil {
		return nil, common.NewErrorf("error reading db: %v", err)
	}

	currentSettings := map[string]string{}
	if opts.PreserveCurrentAccess {
		settingService := &SettingService{}
		for _, key := range []string{"webListen", "webDomain", "webPort", "webCertFile", "webKeyFile", "webBasePath"} {
			value, getErr := settingService.getString(key)
			if getErr == nil {
				currentSettings[key] = value
			}
		}
	}

	if errStop := s.StopXrayService(); errStop != nil {
		logger.Warningf("failed to stop Xray before compatible import: %v", errStop)
	}

	tx := database.GetDB().Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	if err := importLegacyBackupData(tx, data, opts.PreserveCurrentAccess, currentSettings); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}

	s.inboundService.MigrateDB()
	if err := s.RestartXrayService(); err != nil {
		return nil, common.NewErrorf("imported DB but failed to start Xray: %v", err)
	}

	finalSettings := legacySettingsMap(data.Settings)
	if opts.PreserveCurrentAccess {
		for key, value := range currentSettings {
			finalSettings[key] = value
		}
	}
	return &DBImportResult{
		RedirectURL: buildImportRedirectURL(currentScheme, currentHost, finalSettings),
	}, nil
}
