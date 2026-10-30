// Package model defines the database models and data structures used by the 3x-ui panel.
package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/util/json_util"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// Protocol represents the protocol type for Xray inbounds.
type Protocol string

// Protocol constants for different Xray inbound protocols
const (
	VMESS       Protocol = "vmess"
	VLESS       Protocol = "vless"
	Tunnel      Protocol = "tunnel"
	HTTP        Protocol = "http"
	Trojan      Protocol = "trojan"
	Shadowsocks Protocol = "shadowsocks"
	Mixed       Protocol = "mixed"
	WireGuard   Protocol = "wireguard"
	Hysteria    Protocol = "hysteria"
)

// User represents a user account in the 3x-ui panel.
type User struct {
	Id                int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Username          string `json:"username" gorm:"uniqueIndex"`
	Password          string `json:"password"`
	Role              string `json:"role" gorm:"default:admin;index"`
	ParentUserID      *int   `json:"parentUserId,omitempty" gorm:"index"`
	Enabled           bool   `json:"enabled" gorm:"default:true"`
	MaxUsers          int    `json:"maxUsers" gorm:"default:0"`
	MaxTraffic        int64  `json:"maxTraffic" gorm:"default:0"`
	PortalPath        string `json:"portalPath,omitempty" gorm:"size:64;uniqueIndex:idx_reseller_portal_path,where:role = 'reseller' AND portal_path <> ''"`
	PortalPathAliases string `json:"-" gorm:"type:text"`
	Permissions       string `json:"permissions" gorm:"type:text"`
	AllowedInboundIDs string `json:"allowedInboundIds" gorm:"type:text"`
	ManagedState      string `json:"managedState,omitempty" gorm:"type:text"`
	CreatedAt         int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt         int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

// Inbound represents an Xray inbound configuration with traffic statistics and settings.
type Inbound struct {
	Id                   int                  `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`                                                    // Unique identifier
	UserId               int                  `json:"-"`                                                                                               // Associated user ID
	Up                   int64                `json:"up" form:"up"`                                                                                    // Upload traffic in bytes
	Down                 int64                `json:"down" form:"down"`                                                                                // Download traffic in bytes
	Total                int64                `json:"total" form:"total"`                                                                              // Total traffic limit in bytes
	AllTime              int64                `json:"allTime" form:"allTime" gorm:"default:0"`                                                         // All-time traffic usage
	Remark               string               `json:"remark" form:"remark"`                                                                            // Human-readable remark
	Enable               bool                 `json:"enable" form:"enable" gorm:"index:idx_enable_traffic_reset,priority:1"`                           // Whether the inbound is enabled
	ExpiryTime           int64                `json:"expiryTime" form:"expiryTime"`                                                                    // Expiration timestamp
	TrafficReset         string               `json:"trafficReset" form:"trafficReset" gorm:"default:never;index:idx_enable_traffic_reset,priority:2"` // Traffic reset schedule
	LastTrafficResetTime int64                `json:"lastTrafficResetTime" form:"lastTrafficResetTime" gorm:"default:0"`                               // Last traffic reset timestamp
	ClientStats          []xray.ClientTraffic `gorm:"foreignKey:InboundId;references:Id" json:"clientStats" form:"clientStats"`                        // Client traffic statistics

	// Xray configuration fields
	Listen         string   `json:"listen" form:"listen"`
	Port           int      `json:"port" form:"port"`
	Protocol       Protocol `json:"protocol" form:"protocol"`
	Settings       string   `json:"settings" form:"settings"`
	StreamSettings string   `json:"streamSettings" form:"streamSettings"`
	Tag            string   `json:"tag" form:"tag" gorm:"unique"`
	Sniffing       string   `json:"sniffing" form:"sniffing"`
	NodeID         *int     `json:"nodeId,omitempty" form:"nodeId" gorm:"index"`
	NodeInboundID  *int     `json:"nodeInboundId,omitempty" form:"nodeInboundId" gorm:"index"`
}

// OutboundTraffics tracks traffic statistics for Xray outbound connections.
type OutboundTraffics struct {
	Id    int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Tag   string `json:"tag" form:"tag" gorm:"unique"`
	Up    int64  `json:"up" form:"up" gorm:"default:0"`
	Down  int64  `json:"down" form:"down" gorm:"default:0"`
	Total int64  `json:"total" form:"total" gorm:"default:0"`
}

// InboundClientIps stores IP addresses associated with inbound clients for access control.
type InboundClientIps struct {
	Id          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ClientEmail string `json:"clientEmail" form:"clientEmail" gorm:"unique"`
	Ips         string `json:"ips" form:"ips"`
}

// HistoryOfSeeders tracks which database seeders have been executed to prevent re-running.
type HistoryOfSeeders struct {
	Id         int    `json:"id" gorm:"primaryKey;autoIncrement"`
	SeederName string `json:"seederName"`
}

type ApiToken struct {
	Id        int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string `json:"name" gorm:"uniqueIndex;not null"`
	Token     string `json:"token" gorm:"not null"`
	Enabled   bool   `json:"enabled" gorm:"default:true"`
	CreatedAt int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
}

// Node represents a remote panel registered in the central panel.
type Node struct {
	Id            int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Name          string `json:"name" form:"name" gorm:"uniqueIndex"`
	Remark        string `json:"remark" form:"remark"`
	Scheme        string `json:"scheme" form:"scheme"`
	Address       string `json:"address" form:"address"`
	Port          int    `json:"port" form:"port"`
	BasePath      string `json:"basePath" form:"basePath"`
	ApiToken      string `json:"apiToken" form:"apiToken"`
	Enable        bool   `json:"enable" form:"enable" gorm:"default:true"`
	Status        string `json:"status" gorm:"default:unknown"`
	LastHeartbeat int64  `json:"lastHeartbeat"`
	LatencyMs     int    `json:"latencyMs"`
	XrayVersion   string `json:"xrayVersion"`
	PanelVersion  string `json:"panelVersion" gorm:"column:panel_version"`
	LastError     string `json:"lastError"`
	CreatedAt     int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt     int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

// GenXrayInboundConfig generates an Xray inbound configuration from the Inbound model.
func (i *Inbound) GenXrayInboundConfig() *xray.InboundConfig {
	listen := i.Listen
	// Default to 0.0.0.0 (all interfaces) when listen is empty
	// This ensures proper dual-stack IPv4/IPv6 binding in systems where bindv6only=0
	if listen == "" {
		listen = "0.0.0.0"
	}
	listen = fmt.Sprintf("\"%v\"", listen)
	return &xray.InboundConfig{
		Listen:         json_util.RawMessage(listen),
		Port:           i.Port,
		Protocol:       string(i.Protocol),
		Settings:       json_util.RawMessage(i.Settings),
		StreamSettings: json_util.RawMessage(i.StreamSettings),
		Tag:            i.Tag,
		Sniffing:       json_util.RawMessage(i.Sniffing),
	}
}

// Setting stores key-value configuration settings for the 3x-ui panel.
type Setting struct {
	Id    int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Key   string `json:"key" form:"key"`
	Value string `json:"value" form:"value"`
}

type CustomGeoResource struct {
	Id            int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Type          string `json:"type" gorm:"not null;uniqueIndex:idx_custom_geo_type_alias;column:geo_type"`
	Alias         string `json:"alias" gorm:"not null;uniqueIndex:idx_custom_geo_type_alias"`
	Url           string `json:"url" gorm:"not null"`
	LocalPath     string `json:"localPath" gorm:"column:local_path"`
	LastUpdatedAt int64  `json:"lastUpdatedAt" gorm:"default:0;column:last_updated_at"`
	LastModified  string `json:"lastModified" gorm:"column:last_modified"`
	CreatedAt     int64  `json:"createdAt" gorm:"autoCreateTime;column:created_at"`
	UpdatedAt     int64  `json:"updatedAt" gorm:"autoUpdateTime;column:updated_at"`
}

// Client represents a client configuration for Xray inbounds with traffic limits and settings.
type Client struct {
	ID       string `json:"id,omitempty"`       // Unique client identifier
	Security string `json:"security"`           // Security method (e.g., "auto", "aes-128-gcm")
	Password string `json:"password,omitempty"` // Client password
	Flow     string `json:"flow,omitempty"`     // Flow control (XTLS)
	Auth     string `json:"auth,omitempty"`     // Auth password (Hysteria)
	Email    string `json:"email"`              // Client email identifier
	LimitIP  int    `json:"limitIp"`            // IP limit for this client
	// LimitGrace is retained to read existing inbound JSON. Build 41 no longer
	// exposes or writes a client-configurable grace period.
	LimitGrace  int    `json:"limitGrace,omitempty"`                     // Legacy-only compatibility field.
	TotalGB     int64  `json:"totalGB" form:"totalGB"`                   // Nominal traffic allocation in bytes
	QuotaFactor *int64 `json:"quotaFactor,omitempty" form:"quotaFactor"` // Divisor applied to new traffic allocations; 0 disables scaling.
	ExpiryTime  int64  `json:"expiryTime" form:"expiryTime"`             // Expiration timestamp
	Enable      bool   `json:"enable" form:"enable"`                     // Whether the client is enabled
	// DisableReason records why a disabled client was disabled: "manual" (an
	// admin turned it off), "quota" / "expiry" (the traffic job), or
	// "reseller" (the reseller lifecycle). It lives inside the inbound
	// settings JSON, is empty for enabled clients and for clients disabled
	// before the marker existed, and is stripped from the Xray runtime config.
	DisableReason string `json:"disableReason,omitempty"`                  // Why the client is disabled (manual/quota/expiry/reseller)
	TgID          int64  `json:"tgId" form:"tgId"`                         // Telegram user ID for notifications
	SubID         string `json:"subId" form:"subId"`                       // Subscription identifier
	Comment       string `json:"comment" form:"comment"`                   // Client comment
	OwnerUserID   int    `json:"ownerUserId,omitempty" form:"ownerUserId"` // Owning panel user for reseller isolation
	Reset         int    `json:"reset" form:"reset"`                       // Reset period in days
	CreatedAt     int64  `json:"created_at,omitempty"`                     // Creation timestamp
	UpdatedAt     int64  `json:"updated_at,omitempty"`                     // Last update timestamp
}

// UnmarshalJSON accepts legacy Telegram IDs saved as decimal strings as well
// as the numeric form written by current clients. Older panel data contains
// both representations; a single string value must not make the entire
// inbound unreadable (which also prevents client creation and IP-limit policy
// loading for that inbound).
func (c *Client) UnmarshalJSON(data []byte) error {
	type clientAlias Client
	aux := struct {
		*clientAlias
		TgID   json.RawMessage `json:"tgId"`
		Enable json.RawMessage `json:"enable"`
	}{clientAlias: (*clientAlias)(c)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// Older/imported Xray clients may omit the panel-only enable field. Xray
	// treats those clients as active, so preserve that behavior in the panel.
	rawEnable := bytes.TrimSpace(aux.Enable)
	if len(rawEnable) == 0 || bytes.Equal(rawEnable, []byte("null")) {
		c.Enable = true
	} else if err := json.Unmarshal(rawEnable, &c.Enable); err != nil {
		return fmt.Errorf("enable must be a boolean: %w", err)
	}

	rawTgID := bytes.TrimSpace(aux.TgID)
	if len(rawTgID) == 0 || bytes.Equal(rawTgID, []byte("null")) {
		c.TgID = 0
		return nil
	}
	if rawTgID[0] == '"' {
		var text string
		if err := json.Unmarshal(rawTgID, &text); err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			c.TgID = 0
			return nil
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return fmt.Errorf("legacy tgId is not a decimal integer: %w", err)
		}
		c.TgID = value
		return nil
	}

	var value int64
	if err := json.Unmarshal(rawTgID, &value); err != nil {
		return fmt.Errorf("tgId must be an integer or decimal string: %w", err)
	}
	c.TgID = value
	return nil
}
