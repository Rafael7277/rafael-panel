package job

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/observability"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/web/websocket"
	"github.com/mhsanaei/3x-ui/v2/xray"

	"github.com/valyala/fasthttp"
)

// XrayTrafficJob collects and processes traffic statistics from Xray, updating the database and optionally informing external APIs.
type XrayTrafficJob struct {
	settingService  service.SettingService
	xrayService     service.XrayService
	inboundService  service.InboundService
	outboundService service.OutboundService
	runMu           sync.Mutex
	lastMaintenance time.Time
	lastSnapshot    time.Time
	skippedPolls    atomic.Uint64
}

const xrayTrafficMaintenanceInterval = 10 * time.Second

// NewXrayTrafficJob creates a new traffic collection job instance.
func NewXrayTrafficJob() *XrayTrafficJob {
	return new(XrayTrafficJob)
}

// Run collects traffic statistics from Xray and updates the database, triggering restart if needed.
func (j *XrayTrafficJob) Run() {
	if !j.runMu.TryLock() {
		if j.skippedPolls.Add(1)%30 == 1 {
			logger.Warning("PERF repeated Xray traffic poll overlap; quota sampling may be delayed")
		}
		return
	}
	j.skippedPolls.Store(0)
	defer j.runMu.Unlock()
	runStarted := time.Now()
	if !j.xrayService.IsXrayRunning() {
		return
	}
	phaseStarted := time.Now()
	traffics, clientTraffics, err := j.xrayService.GetXrayTraffic()
	if err != nil {
		observability.ReportSlow("traffic.collect", time.Since(phaseStarted), "result=error")
		return
	}
	observability.ReportSlow("traffic.collect", time.Since(phaseStarted), fmt.Sprintf("inbounds=%d clients=%d", len(traffics), len(clientTraffics)))
	phaseStarted = time.Now()
	err, needRestart0 := j.inboundService.AddTraffic(traffics, clientTraffics)
	observability.ReportSlow("traffic.inbound_reconcile", time.Since(phaseStarted), fmt.Sprintf("inbounds=%d clients=%d", len(traffics), len(clientTraffics)))
	if err != nil {
		logger.Warning("add inbound traffic failed:", err)
	}
	phaseStarted = time.Now()
	err, needRestart1 := j.outboundService.AddTraffic(traffics, clientTraffics)
	observability.ReportSlow("traffic.outbound_reconcile", time.Since(phaseStarted), fmt.Sprintf("inbounds=%d clients=%d", len(traffics), len(clientTraffics)))
	if err != nil {
		logger.Warning("add outbound traffic failed:", err)
	}
	now := time.Now()
	maintenanceDue := j.lastMaintenance.IsZero() || now.Sub(j.lastMaintenance) >= xrayTrafficMaintenanceInterval
	if maintenanceDue {
		phaseStarted = time.Now()
		if err := (&service.UserService{}).SyncAllResellerRuntimeStates(); err != nil {
			logger.Warning("sync reseller runtime states failed:", err)
		}
		observability.ReportSlow("traffic.reseller_sync", time.Since(phaseStarted), "")

		phaseStarted = time.Now()
		if ExternalTrafficInformEnable, err := j.settingService.GetExternalTrafficInformEnable(); ExternalTrafficInformEnable {
			j.informTrafficToExternalAPI(traffics, clientTraffics)
		} else if err != nil {
			logger.Warning("get ExternalTrafficInformEnable failed:", err)
		}
		observability.ReportSlow("traffic.external_report", time.Since(phaseStarted), "")
		j.lastMaintenance = now
	}
	if needRestart0 || needRestart1 {
		j.xrayService.SetToNeedRestart()
	}

	// If no frontend client is connected, skip all WebSocket broadcasting routines,
	// including expensive DB queries for online clients and JSON marshaling.
	if !websocket.HasClients() {
		observability.ReportSlow("traffic.poll", time.Since(runStarted), fmt.Sprintf("inbounds=%d clients=%d websocket_clients=0", len(traffics), len(clientTraffics)))
		return
	}

	// Update online clients list and map
	phaseStarted = time.Now()
	onlineClients := j.inboundService.GetOnlineClients()
	lastOnlineMap, err := j.inboundService.GetClientsLastOnline()
	if err != nil {
		logger.Warning("get clients last online failed:", err)
		lastOnlineMap = make(map[string]int64)
	}
	observability.ReportSlow("traffic.online_clients", time.Since(phaseStarted), "")

	// Broadcast traffic update (deltas and online stats) via WebSocket
	trafficUpdate := map[string]any{
		"traffics":       traffics,
		"clientTraffics": clientTraffics,
		"onlineClients":  onlineClients,
		"lastOnlineMap":  lastOnlineMap,
	}
	websocket.BroadcastTraffic(trafficUpdate)

	if j.lastSnapshot.IsZero() || time.Since(j.lastSnapshot) >= xrayTrafficMaintenanceInterval {
		phaseStarted = time.Now()
		// Fetch full snapshots less frequently than quota sampling to keep
		// WebSocket refresh cost bounded while preserving quick quota checks.
		updatedInbounds, err := j.inboundService.GetAllInbounds()
		if err != nil {
			logger.Warning("get all inbounds for websocket failed:", err)
		}

		updatedOutbounds, err := j.outboundService.GetOutboundsTraffic()
		if err != nil {
			logger.Warning("get all outbounds for websocket failed:", err)
		}

		// The WebSocket hub checks payload size and sends a lightweight
		// invalidation signal instead of an oversized full snapshot.
		if updatedInbounds != nil {
			websocket.BroadcastInbounds(updatedInbounds)
		}

		if updatedOutbounds != nil {
			websocket.BroadcastOutbounds(updatedOutbounds)
		}
		observability.ReportSlow("traffic.websocket_snapshot", time.Since(phaseStarted), fmt.Sprintf("inbounds=%d clients=%d", len(updatedInbounds), len(updatedOutbounds)))
		j.lastSnapshot = time.Now()
	}
	observability.ReportSlow("traffic.poll", time.Since(runStarted), fmt.Sprintf("inbounds=%d clients=%d websocket_clients=1", len(traffics), len(clientTraffics)))
}

func (j *XrayTrafficJob) informTrafficToExternalAPI(inboundTraffics []*xray.Traffic, clientTraffics []*xray.ClientTraffic) {
	informURL, err := j.settingService.GetExternalTrafficInformURI()
	if err != nil {
		logger.Warning("get ExternalTrafficInformURI failed:", err)
		return
	}
	requestBody, err := json.Marshal(map[string]any{"clientTraffics": clientTraffics, "inboundTraffics": inboundTraffics})
	if err != nil {
		logger.Warning("parse client/inbound traffic failed:", err)
		return
	}
	request := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(request)
	request.Header.SetMethod("POST")
	request.Header.SetContentType("application/json; charset=UTF-8")
	request.SetBody([]byte(requestBody))
	request.SetRequestURI(informURL)
	response := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(response)
	if err := fasthttp.Do(request, response); err != nil {
		logger.Warning("POST ExternalTrafficInformURI failed:", err)
	}
}
