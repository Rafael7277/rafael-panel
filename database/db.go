// Package database provides database initialization, migration, and management utilities
// for the 3x-ui panel using GORM with SQLite.
package database

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/util/crypto"
	"github.com/mhsanaei/3x-ui/v2/xray"
	_ "modernc.org/sqlite"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var db *gorm.DB

const (
	DialectSQLite   = "sqlite"
	DialectPostgres = "postgres"
)

// IsPostgres reports whether the active connection is a PostgreSQL backend.
func IsPostgres() bool {
	if db == nil {
		return config.GetDBKind() == DialectPostgres
	}
	return db.Dialector.Name() == DialectPostgres
}

// Dialect returns the active GORM dialect name, or "" if the DB is not open.
func Dialect() string {
	if db == nil {
		return ""
	}
	return db.Dialector.Name()
}

const (
	defaultUsername = "admin"
	defaultPassword = "admin"
)

func initModels() error {
	models := []any{
		&model.User{},
		&model.Inbound{},
		&model.OutboundTraffics{},
		&model.Setting{},
		&model.InboundClientIps{},
		&xray.ClientTraffic{},
		&model.HistoryOfSeeders{},
		&model.ApiToken{},
		&model.CustomGeoResource{},
		&model.Node{},
		&model.NodeClientTraffic{},
	}
	for _, model := range models {
		if err := db.AutoMigrate(model); err != nil {
			log.Printf("Error auto migrating model: %v", err)
			return err
		}
	}
	return nil
}

func normalizeLegacyData() error {
	if err := db.Model(&model.User{}).
		Where("COALESCE(role, '') = ''").
		Updates(map[string]any{
			"role":    defaultUsernameToRole(),
			"enabled": true,
		}).Error; err != nil {
		return err
	}

	if err := ensureRoutingServiceInTemplate(); err != nil {
		return err
	}

	var basePathSetting model.Setting
	err := db.Model(&model.Setting{}).Where("key = ?", "webBasePath").First(&basePathSetting).Error
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	basePath := strings.TrimSpace(basePathSetting.Value)
	if basePath == "" {
		basePath = "/"
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}
	if basePath == basePathSetting.Value {
		return nil
	}
	return db.Model(&model.Setting{}).Where("id = ?", basePathSetting.Id).Update("value", basePath).Error
}

func defaultUsernameToRole() string {
	return "admin"
}

// ensureRoutingServiceInTemplate adds "RoutingService" to the api.services
// array of the stored xrayTemplateConfig (design: iplimit.md). The sticky
// IP-limit router installs dynamic blackhole routing rules through that
// gRPC service; without it the router stays in kick-only fallback mode.
// Idempotent: no-op when the service is already listed, when the template
// has no api block, or when no template row exists (fresh installs get the
// new default from defaultValueMap). A malformed template is left untouched
// and reported, never rewritten.
func ensureRoutingServiceInTemplate() error {
	var template model.Setting
	err := db.Model(&model.Setting{}).Where("key = ?", "xrayTemplateConfig").First(&template).Error
	if IsNotFound(err) {
		return nil // fresh install: default template already carries it
	}
	if err != nil {
		return err
	}

	var cfg struct {
		API struct {
			Services []string `json:"services"`
		} `json:"api"`
	}
	if err := json.Unmarshal([]byte(template.Value), &cfg); err != nil {
		log.Printf("xrayTemplateConfig migration: template is not valid JSON, skipping: %v", err)
		return nil
	}
	if slices.Contains(cfg.API.Services, "RoutingService") {
		return nil
	}
	cfg.API.Services = append(cfg.API.Services, "RoutingService")

	// Patch only the api.services array inside the original template text
	// so every other block (comments order, unknown keys) survives intact.
	var full map[string]any
	if err := json.Unmarshal([]byte(template.Value), &full); err != nil {
		return err
	}
	apiBlock, ok := full["api"].(map[string]any)
	if !ok {
		// Template without an api block: nothing to enable; the runtime
		// config would have no gRPC API at all, so leave it alone.
		return nil
	}
	services := make([]any, 0, len(cfg.API.Services))
	for _, s := range cfg.API.Services {
		services = append(services, s)
	}
	apiBlock["services"] = services

	updated, err := json.Marshal(full)
	if err != nil {
		return err
	}
	return db.Model(&model.Setting{}).Where("id = ?", template.Id).
		Update("value", string(updated)).Error
}

// initUser creates a default admin user if the users table is empty.
func initUser() error {
	empty, err := isTableEmpty("users")
	if err != nil {
		log.Printf("Error checking if users table is empty: %v", err)
		return err
	}
	if empty {
		hashedPassword, err := crypto.HashPasswordAsBcrypt(defaultPassword)

		if err != nil {
			log.Printf("Error hashing default password: %v", err)
			return err
		}

		user := &model.User{
			Username: defaultUsername,
			Password: hashedPassword,
			Role:     "admin",
			Enabled:  true,
		}
		return db.Create(user).Error
	}
	return nil
}

// runSeeders migrates user passwords to bcrypt and records seeder execution to prevent re-running.
func runSeeders(isUsersEmpty bool) error {
	empty, err := isTableEmpty("history_of_seeders")
	if err != nil {
		log.Printf("Error checking if users table is empty: %v", err)
		return err
	}

	if empty && isUsersEmpty {
		hashSeeder := &model.HistoryOfSeeders{
			SeederName: "UserPasswordHash",
		}
		return db.Create(hashSeeder).Error
	} else {
		var seedersHistory []string
		db.Model(&model.HistoryOfSeeders{}).Pluck("seeder_name", &seedersHistory)

		if !slices.Contains(seedersHistory, "UserPasswordHash") && !isUsersEmpty {
			var users []model.User
			db.Find(&users)

			for _, user := range users {
				hashedPassword, err := crypto.HashPasswordAsBcrypt(user.Password)
				if err != nil {
					log.Printf("Error hashing password for user '%s': %v", user.Username, err)
					return err
				}
				db.Model(&user).Update("password", hashedPassword)
			}

			hashSeeder := &model.HistoryOfSeeders{
				SeederName: "UserPasswordHash",
			}
			return db.Create(hashSeeder).Error
		}
	}

	return nil
}

// isTableEmpty returns true if the named table contains zero rows.
func isTableEmpty(tableName string) (bool, error) {
	var count int64
	err := db.Table(tableName).Count(&count).Error
	return count == 0, err
}

// InitDB sets up the database connection, migrates models, and runs seeders.
func InitDB(dbPath string) error {
	var err error
	if config.GetDBKind() != DialectPostgres {
		dir := path.Dir(dbPath)
		err = os.MkdirAll(dir, fs.ModePerm)
		if err != nil {
			return err
		}
	}

	var gormLogger logger.Interface

	if config.IsDebug() {
		gormLogger = logger.Default
	} else {
		gormLogger = logger.Discard
	}

	c := &gorm.Config{
		Logger: gormLogger,
	}

	switch config.GetDBKind() {
	case DialectPostgres:
		dsn := config.GetDBDSN()
		if dsn == "" {
			return errors.New("XUI_DB_TYPE=postgres but XUI_DB_DSN is empty")
		}
		db, err = gorm.Open(postgres.Open(dsn), c)
	default:
		db, err = gorm.Open(sqlite.Dialector{
			DriverName: "sqlite",
			DSN:        dbPath,
		}, c)
	}
	if err != nil {
		return err
	}

	if err := initModels(); err != nil {
		return err
	}
	if err := normalizeLegacyData(); err != nil {
		return err
	}

	isUsersEmpty, err := isTableEmpty("users")
	if err != nil {
		return err
	}

	if err := initUser(); err != nil {
		return err
	}
	return runSeeders(isUsersEmpty)
}

// CloseDB closes the database connection if it exists.
func CloseDB() error {
	if db != nil {
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		return sqlDB.Close()
	}
	return nil
}

// GetDB returns the global GORM database instance.
func GetDB() *gorm.DB {
	return db
}

func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// IsSQLiteDB checks if the given file is a valid SQLite database by reading its signature.
func IsSQLiteDB(file io.ReaderAt) (bool, error) {
	signature := []byte("SQLite format 3\x00")
	buf := make([]byte, len(signature))
	_, err := file.ReadAt(buf, 0)
	if err != nil {
		return false, err
	}
	return bytes.Equal(buf, signature), nil
}

// Checkpoint performs a WAL checkpoint on the SQLite database to ensure data consistency.
func Checkpoint() error {
	if IsPostgres() {
		return nil
	}
	// Update WAL
	err := db.Exec("PRAGMA wal_checkpoint;").Error
	if err != nil {
		return err
	}
	return nil
}

// ValidateSQLiteDB opens the provided sqlite DB path with a throw-away connection
// and runs a PRAGMA integrity_check to ensure the file is structurally sound.
// It does not mutate global state or run migrations.
func ValidateSQLiteDB(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil { // file must exist
		return err
	}
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if err := sqlDB.Ping(); err != nil {
		return err
	}
	row := sqlDB.QueryRow("PRAGMA integrity_check;")
	var res string
	if err := row.Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New("sqlite integrity check failed: " + res)
	}
	return nil
}
