package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/common"
	"github.com/mhsanaei/3x-ui/v2/xray"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type HeartbeatPatch struct {
	Status        string
	LastHeartbeat int64
	LatencyMs     int
	XrayVersion   string
	PanelVersion  string
	LastError     string
}

type ProbeResultUI struct {
	Status       string `json:"status"`
	LatencyMs    int    `json:"latencyMs"`
	XrayVersion  string `json:"xrayVersion"`
	PanelVersion string `json:"panelVersion"`
	Error        string `json:"error"`
}

type remoteEnvelope struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

type NodeService struct{}

func (s *NodeService) GetAll() ([]*model.Node, error) {
	var nodes []*model.Node
	err := database.GetDB().Model(model.Node{}).Order("id asc").Find(&nodes).Error
	return nodes, err
}

func (s *NodeService) GetById(id int) (*model.Node, error) {
	n := &model.Node{}
	if err := database.GetDB().Model(model.Node{}).Where("id = ?", id).First(n).Error; err != nil {
		return nil, err
	}
	return n, nil
}

func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (s *NodeService) normalize(n *model.Node) error {
	n.Name = strings.TrimSpace(n.Name)
	n.Address = strings.TrimSpace(n.Address)
	n.ApiToken = strings.TrimSpace(n.ApiToken)
	n.Remark = strings.TrimSpace(n.Remark)
	n.BasePath = normalizeBasePath(n.BasePath)
	if n.Name == "" {
		return common.NewError("node name is required")
	}
	if n.Address == "" {
		return common.NewError("node address is required")
	}
	if n.Port <= 0 || n.Port > 65535 {
		return common.NewError("node port must be 1-65535")
	}
	if n.Scheme != "http" && n.Scheme != "https" {
		n.Scheme = "https"
	}
	return nil
}

func (s *NodeService) Create(n *model.Node) error {
	if err := s.normalize(n); err != nil {
		return err
	}
	return database.GetDB().Create(n).Error
}

func (s *NodeService) Update(id int, in *model.Node) error {
	if err := s.normalize(in); err != nil {
		return err
	}
	updates := map[string]any{
		"name":       in.Name,
		"remark":     in.Remark,
		"scheme":     in.Scheme,
		"address":    in.Address,
		"port":       in.Port,
		"base_path":  in.BasePath,
		"api_token":  in.ApiToken,
		"enable":     in.Enable,
		"updated_at": time.Now().UnixMilli(),
	}
	return database.GetDB().Model(model.Node{}).Where("id = ?", id).Updates(updates).Error
}

func (s *NodeService) Delete(id int) error {
	db := database.GetDB()
	if err := db.Where("node_id = ?", id).Delete(&model.Inbound{}).Error; err != nil {
		return err
	}
	if err := db.Where("node_id = ?", id).Delete(&model.NodeClientTraffic{}).Error; err != nil {
		return err
	}
	return db.Delete(model.Node{}, id).Error
}

func (s *NodeService) SetEnable(id int, enable bool) error {
	return database.GetDB().Model(model.Node{}).Where("id = ?", id).Update("enable", enable).Error
}

func (s *NodeService) UpdateHeartbeat(id int, p HeartbeatPatch) error {
	updates := map[string]any{
		"status":         p.Status,
		"last_heartbeat": p.LastHeartbeat,
		"latency_ms":     p.LatencyMs,
		"xray_version":   p.XrayVersion,
		"panel_version":  p.PanelVersion,
		"last_error":     p.LastError,
	}
	return database.GetDB().Model(model.Node{}).Where("id = ?", id).Updates(updates).Error
}

func (s *NodeService) endpointURL(n *model.Node, path string) string {
	return (&url.URL{
		Scheme: n.Scheme,
		Host:   net.JoinHostPort(n.Address, strconv.Itoa(n.Port)),
		Path:   n.BasePath + strings.TrimPrefix(path, "/"),
	}).String()
}

func (s *NodeService) doJSON(ctx context.Context, n *model.Node, method string, path string, body any, out any) error {
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, s.endpointURL(n, path), payload)
	if err != nil {
		return err
	}
	if n.ApiToken != "" {
		req.Header.Set("Authorization", "Bearer "+n.ApiToken)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New(fmt.Sprintf("HTTP %d from remote panel", resp.StatusCode))
	}

	var envelope remoteEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if !envelope.Success {
		if envelope.Msg == "" {
			envelope.Msg = "remote operation failed"
		}
		return errors.New(envelope.Msg)
	}
	if out != nil && len(envelope.Obj) > 0 && string(envelope.Obj) != "null" {
		return json.Unmarshal(envelope.Obj, out)
	}
	return nil
}

func (s *NodeService) Probe(ctx context.Context, n *model.Node) (HeartbeatPatch, error) {
	patch := HeartbeatPatch{LastHeartbeat: time.Now().Unix()}
	if err := s.normalize(n); err != nil {
		patch.LastError = err.Error()
		return patch, err
	}

	start := time.Now()
	var obj struct {
		Xray struct {
			Version string `json:"version"`
		} `json:"xray"`
		PanelVersion string `json:"panelVersion"`
	}
	if err := s.doJSON(ctx, n, http.MethodGet, "panel/api/server/status", nil, &obj); err != nil {
		patch.LastError = FriendlyProbeError(err.Error())
		return patch, err
	}
	patch.LatencyMs = int(time.Since(start) / time.Millisecond)
	patch.Status = "online"
	patch.XrayVersion = obj.Xray.Version
	patch.PanelVersion = obj.PanelVersion
	return patch, nil
}

func (s *NodeService) fetchRemoteInbounds(ctx context.Context, n *model.Node) ([]*model.Inbound, error) {
	var inbounds []*model.Inbound
	if err := s.doJSON(ctx, n, http.MethodGet, "panel/api/inbounds/list", nil, &inbounds); err != nil {
		return nil, err
	}
	return inbounds, nil
}

func (s *NodeService) defaultUserID(tx *gorm.DB) int {
	var user model.User
	if err := tx.Order("id asc").First(&user).Error; err == nil && user.Id > 0 {
		return user.Id
	}
	return 1
}

func (s *NodeService) upsertRemoteInbound(tx *gorm.DB, nodeID int, userID int, remoteInbound *model.Inbound) (*model.Inbound, error) {
	local := &model.Inbound{}
	err := tx.Where("node_id = ? AND node_inbound_id = ?", nodeID, remoteInbound.Id).First(local).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	remoteID := remoteInbound.Id
	if errors.Is(err, gorm.ErrRecordNotFound) {
		local = &model.Inbound{}
	}

	local.UserId = userID
	local.Up = remoteInbound.Up
	local.Down = remoteInbound.Down
	local.Total = remoteInbound.Total
	local.AllTime = remoteInbound.AllTime
	local.Remark = remoteInbound.Remark
	local.Enable = remoteInbound.Enable
	local.ExpiryTime = remoteInbound.ExpiryTime
	local.TrafficReset = remoteInbound.TrafficReset
	local.LastTrafficResetTime = remoteInbound.LastTrafficResetTime
	local.Listen = remoteInbound.Listen
	local.Port = remoteInbound.Port
	local.Protocol = remoteInbound.Protocol
	local.Settings = remoteInbound.Settings
	local.StreamSettings = remoteInbound.StreamSettings
	local.Tag = remoteInbound.Tag
	local.Sniffing = remoteInbound.Sniffing
	local.NodeID = &nodeID
	local.NodeInboundID = &remoteID

	if local.Id == 0 {
		if err := tx.Create(local).Error; err != nil {
			return nil, err
		}
		return local, nil
	}
	if err := tx.Save(local).Error; err != nil {
		return nil, err
	}
	return local, nil
}

func (s *NodeService) syncRemoteClientTraffics(tx *gorm.DB, nodeID int, localInbound *model.Inbound, remoteInbound *model.Inbound) error {
	seenEmails := make(map[string]struct{}, len(remoteInbound.ClientStats))
	for _, stat := range remoteInbound.ClientStats {
		if strings.TrimSpace(stat.Email) == "" {
			continue
		}
		seenEmails[stat.Email] = struct{}{}
		remoteStat := stat
		remoteStat.InboundId = localInbound.Id
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "email"}},
			DoUpdates: clause.AssignmentColumns([]string{"inbound_id", "enable", "up", "down", "all_time", "expiry_time", "total", "reset", "last_online"}),
		}).Create(&remoteStat).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "node_id"}, {Name: "email"}},
			DoUpdates: clause.AssignmentColumns([]string{"up", "down"}),
		}).Create(&model.NodeClientTraffic{
			NodeId: nodeID,
			Email:  stat.Email,
			Up:     stat.Up,
			Down:   stat.Down,
		}).Error; err != nil {
			return err
		}
	}

	var existing []xray.ClientTraffic
	if err := tx.Where("inbound_id = ?", localInbound.Id).Find(&existing).Error; err != nil {
		return err
	}
	for _, traffic := range existing {
		if _, ok := seenEmails[traffic.Email]; ok {
			continue
		}
		if err := tx.Where("email = ?", traffic.Email).Delete(&xray.ClientTraffic{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ? AND email = ?", nodeID, traffic.Email).Delete(&model.NodeClientTraffic{}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *NodeService) SyncNode(ctx context.Context, id int) ([]*model.Inbound, error) {
	n, err := s.GetById(id)
	if err != nil {
		return nil, err
	}
	patch, probeErr := s.Probe(ctx, n)
	if probeErr != nil {
		patch.Status = "offline"
		_ = s.UpdateHeartbeat(id, patch)
		return nil, probeErr
	}
	_ = s.UpdateHeartbeat(id, patch)

	remoteInbounds, err := s.fetchRemoteInbounds(ctx, n)
	if err != nil {
		return nil, err
	}

	db := database.GetDB()
	tx := db.Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}

	userID := s.defaultUserID(tx)
	seenRemoteIDs := make(map[int]struct{}, len(remoteInbounds))
	localInbounds := make([]*model.Inbound, 0, len(remoteInbounds))
	for _, remoteInbound := range remoteInbounds {
		seenRemoteIDs[remoteInbound.Id] = struct{}{}
		localInbound, err := s.upsertRemoteInbound(tx, id, userID, remoteInbound)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := s.syncRemoteClientTraffics(tx, id, localInbound, remoteInbound); err != nil {
			tx.Rollback()
			return nil, err
		}
		localInbounds = append(localInbounds, localInbound)
	}

	var stale []*model.Inbound
	if err := tx.Where("node_id = ?", id).Find(&stale).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	for _, inbound := range stale {
		if inbound.NodeInboundID == nil {
			continue
		}
		if _, ok := seenRemoteIDs[*inbound.NodeInboundID]; ok {
			continue
		}
		if err := tx.Where("inbound_id = ?", inbound.Id).Delete(&xray.ClientTraffic{}).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Delete(&model.Inbound{}, inbound.Id).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return localInbounds, nil
}

func (s *NodeService) SyncAll(ctx context.Context) error {
	nodes, err := s.GetAll()
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if !n.Enable {
			continue
		}
		if _, err := s.SyncNode(ctx, n.Id); err != nil {
			logger.Warningf("sync node %d failed: %v", n.Id, err)
		}
	}
	return nil
}

func (s *NodeService) CreateRemoteInbound(ctx context.Context, n *model.Node, inbound *model.Inbound) (*model.Inbound, error) {
	var created model.Inbound
	if err := s.doJSON(ctx, n, http.MethodPost, "panel/api/inbounds/add", inbound, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

func (s *NodeService) UpdateRemoteInbound(ctx context.Context, n *model.Node, remoteID int, inbound *model.Inbound) (*model.Inbound, error) {
	var updated model.Inbound
	if err := s.doJSON(ctx, n, http.MethodPost, fmt.Sprintf("panel/api/inbounds/update/%d", remoteID), inbound, &updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

func (s *NodeService) DeleteRemoteInbound(ctx context.Context, n *model.Node, remoteID int) error {
	return s.doJSON(ctx, n, http.MethodPost, fmt.Sprintf("panel/api/inbounds/del/%d", remoteID), nil, nil)
}

func (s *NodeService) AddRemoteClient(ctx context.Context, n *model.Node, payload *model.Inbound) error {
	return s.doJSON(ctx, n, http.MethodPost, "panel/api/inbounds/addClient", payload, nil)
}

func (s *NodeService) UpdateRemoteClient(ctx context.Context, n *model.Node, clientID string, payload *model.Inbound) error {
	return s.doJSON(ctx, n, http.MethodPost, "panel/api/inbounds/updateClient/"+url.PathEscape(clientID), payload, nil)
}

func (s *NodeService) DeleteRemoteClient(ctx context.Context, n *model.Node, remoteInboundID int, clientID string) error {
	return s.doJSON(ctx, n, http.MethodPost, fmt.Sprintf("panel/api/inbounds/%d/delClient/%s", remoteInboundID, url.PathEscape(clientID)), nil, nil)
}

func (s *NodeService) DeleteRemoteClientByEmail(ctx context.Context, n *model.Node, remoteInboundID int, email string) error {
	return s.doJSON(ctx, n, http.MethodPost, fmt.Sprintf("panel/api/inbounds/%d/delClientByEmail/%s", remoteInboundID, url.PathEscape(email)), nil, nil)
}

func (s *NodeService) ResetRemoteClientTraffic(ctx context.Context, n *model.Node, remoteInboundID int, email string) error {
	return s.doJSON(ctx, n, http.MethodPost, fmt.Sprintf("panel/api/inbounds/%d/resetClientTraffic/%s", remoteInboundID, url.PathEscape(email)), nil, nil)
}

func (s *NodeService) ResetRemoteAllClientTraffic(ctx context.Context, n *model.Node, remoteInboundID int) error {
	return s.doJSON(ctx, n, http.MethodPost, fmt.Sprintf("panel/api/inbounds/resetAllClientTraffics/%d", remoteInboundID), nil, nil)
}

func (p HeartbeatPatch) ToUI(ok bool) ProbeResultUI {
	r := ProbeResultUI{
		LatencyMs:    p.LatencyMs,
		XrayVersion:  p.XrayVersion,
		PanelVersion: p.PanelVersion,
		Error:        p.LastError,
	}
	if ok {
		r.Status = "online"
	} else {
		r.Status = "offline"
	}
	return r
}

func FriendlyProbeError(msg string) string {
	if strings.Contains(msg, "server gave HTTP response to HTTPS client") {
		return "the server speaks HTTP, not HTTPS; set the node scheme to http"
	}
	return msg
}
