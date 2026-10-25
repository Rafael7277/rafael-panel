// Package xray provides integration with the Xray proxy core.
// It includes API client functionality, configuration management, traffic monitoring,
// and process control for Xray instances.
package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/util/common"

	"github.com/xtls/xray-core/app/proxyman/command"
	routerCommand "github.com/xtls/xray-core/app/router/command"
	statsService "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/infra/conf"
	confserial "github.com/xtls/xray-core/infra/conf/serial"
	hysteriaAccount "github.com/xtls/xray-core/proxy/hysteria/account"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"github.com/xtls/xray-core/proxy/trojan"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vmess"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// XrayAPI is a gRPC client for managing Xray core configuration, inbounds, outbounds, and statistics.
type XrayAPI struct {
	HandlerServiceClient *command.HandlerServiceClient
	StatsServiceClient   *statsService.StatsServiceClient
	RoutingServiceClient *routerCommand.RoutingServiceClient
	grpcClient           *grpc.ClientConn
	isConnected          bool
}

func getRequiredUserString(user map[string]any, key string) (string, error) {
	value, ok := user[key]
	if !ok || value == nil {
		return "", fmt.Errorf("missing required user field %q", key)
	}

	strValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("invalid type for user field %q: %T", key, value)
	}

	return strValue, nil
}

func getOptionalUserString(user map[string]any, key string) (string, error) {
	value, ok := user[key]
	if !ok || value == nil {
		return "", nil
	}

	strValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("invalid type for user field %q: %T", key, value)
	}

	return strValue, nil
}

// Init connects to the Xray API server and initializes handler and stats service clients.
func (x *XrayAPI) Init(apiPort int) error {
	if apiPort <= 0 || apiPort > math.MaxUint16 {
		return fmt.Errorf("invalid Xray API port: %d", apiPort)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", apiPort)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to Xray API: %w", err)
	}

	x.grpcClient = conn
	x.isConnected = true

	hsClient := command.NewHandlerServiceClient(conn)
	ssClient := statsService.NewStatsServiceClient(conn)
	rsClient := routerCommand.NewRoutingServiceClient(conn)

	x.HandlerServiceClient = &hsClient
	x.StatsServiceClient = &ssClient
	x.RoutingServiceClient = &rsClient

	return nil
}

// Close closes the gRPC connection and resets the XrayAPI client state.
func (x *XrayAPI) Close() {
	if x.grpcClient != nil {
		x.grpcClient.Close()
	}
	x.HandlerServiceClient = nil
	x.StatsServiceClient = nil
	x.RoutingServiceClient = nil
	x.isConnected = false
}

// AddInbound adds a new inbound configuration to the Xray core via gRPC.
func (x *XrayAPI) AddInbound(inbound []byte) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray api is not initialized")
	}
	client := *x.HandlerServiceClient

	conf := new(conf.InboundDetourConfig)
	err := json.Unmarshal(inbound, conf)
	if err != nil {
		logger.Debug("Failed to unmarshal inbound:", err)
		return err
	}
	config, err := conf.Build()
	if err != nil {
		logger.Debug("Failed to build inbound Detur:", err)
		return err
	}
	inboundConfig := command.AddInboundRequest{Inbound: config}

	_, err = client.AddInbound(context.Background(), &inboundConfig)

	return err
}

// DelInbound removes an inbound configuration from the Xray core by tag.
func (x *XrayAPI) DelInbound(tag string) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray api is not initialized")
	}
	client := *x.HandlerServiceClient
	_, err := client.RemoveInbound(context.Background(), &command.RemoveInboundRequest{
		Tag: tag,
	})
	return err
}

// AddUser adds a user to an inbound in the Xray core using the specified protocol and user data.
func (x *XrayAPI) AddUser(Protocol string, inboundTag string, user map[string]any) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray api is not initialized")
	}
	userEmail, err := getRequiredUserString(user, "email")
	if err != nil {
		return err
	}

	var account *serial.TypedMessage
	switch Protocol {
	case "vmess":
		userID, err := getRequiredUserString(user, "id")
		if err != nil {
			return err
		}

		account = serial.ToTypedMessage(&vmess.Account{
			Id: userID,
		})
	case "vless":
		userID, err := getRequiredUserString(user, "id")
		if err != nil {
			return err
		}

		userFlow, err := getOptionalUserString(user, "flow")
		if err != nil {
			return err
		}

		vlessAccount := &vless.Account{
			Id:   userID,
			Flow: userFlow,
		}
		// Add testseed if provided
		if testseedVal, ok := user["testseed"]; ok {
			if testseedArr, ok := testseedVal.([]any); ok && len(testseedArr) >= 4 {
				testseed := make([]uint32, len(testseedArr))
				for i, v := range testseedArr {
					if num, ok := v.(float64); ok {
						testseed[i] = uint32(num)
					}
				}
				vlessAccount.Testseed = testseed
			} else if testseedArr, ok := testseedVal.([]uint32); ok && len(testseedArr) >= 4 {
				vlessAccount.Testseed = testseedArr
			}
		}
		// Add testpre if provided (for outbound, but can be in user for compatibility)
		if testpreVal, ok := user["testpre"]; ok {
			if testpre, ok := testpreVal.(float64); ok && testpre > 0 {
				vlessAccount.Testpre = uint32(testpre)
			} else if testpre, ok := testpreVal.(uint32); ok && testpre > 0 {
				vlessAccount.Testpre = testpre
			}
		}
		account = serial.ToTypedMessage(vlessAccount)
	case "trojan":
		password, err := getRequiredUserString(user, "password")
		if err != nil {
			return err
		}

		account = serial.ToTypedMessage(&trojan.Account{
			Password: password,
		})
	case "shadowsocks":
		cipher, err := getOptionalUserString(user, "cipher")
		if err != nil {
			return err
		}

		password, err := getRequiredUserString(user, "password")
		if err != nil {
			return err
		}

		var ssCipherType shadowsocks.CipherType
		switch cipher {
		case "aes-128-gcm":
			ssCipherType = shadowsocks.CipherType_AES_128_GCM
		case "aes-256-gcm":
			ssCipherType = shadowsocks.CipherType_AES_256_GCM
		case "chacha20-poly1305", "chacha20-ietf-poly1305":
			ssCipherType = shadowsocks.CipherType_CHACHA20_POLY1305
		case "xchacha20-poly1305", "xchacha20-ietf-poly1305":
			ssCipherType = shadowsocks.CipherType_XCHACHA20_POLY1305
		default:
			ssCipherType = shadowsocks.CipherType_NONE
		}

		if ssCipherType != shadowsocks.CipherType_NONE {
			account = serial.ToTypedMessage(&shadowsocks.Account{
				Password:   password,
				CipherType: ssCipherType,
			})
		} else {
			account = serial.ToTypedMessage(&shadowsocks_2022.ServerConfig{
				Key:   password,
				Email: userEmail,
			})
		}
	case "hysteria":
		auth, err := getRequiredUserString(user, "auth")
		if err != nil {
			return err
		}

		account = serial.ToTypedMessage(&hysteriaAccount.Account{
			Auth: auth,
		})
	default:
		return nil
	}

	client := *x.HandlerServiceClient

	_, err = client.AlterInbound(context.Background(), &command.AlterInboundRequest{
		Tag: inboundTag,
		Operation: serial.ToTypedMessage(&command.AddUserOperation{
			User: &protocol.User{
				Email:   userEmail,
				Account: account,
			},
		}),
	})
	return err
}

// RemoveUser removes a user from an inbound in the Xray core by email.
func (x *XrayAPI) RemoveUser(inboundTag, email string) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray api is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	op := &command.RemoveUserOperation{Email: email}
	req := &command.AlterInboundRequest{
		Tag:       inboundTag,
		Operation: serial.ToTypedMessage(op),
	}

	_, err := (*x.HandlerServiceClient).AlterInbound(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to remove user: %w", err)
	}

	return nil
}

// AddRoutingRule installs a dynamic routing rule in the Xray core that
// sends every NEW connection matching userEmail AND source ip to the
// "blocked" (blackhole) outbound. The rule carries the given ruleTag so it
// can be removed again with RemoveRoutingRule.
//
// Pattern follows Xray's own `xray api sib` command (main/commands/all/api/
// source_ip_block.go): build the rule as JSON, decode through infra/conf,
// Build() it into a *router.Config, wrap in a TypedMessage and AddRule it.
// ShouldAppend MUST stay true: with false, Xray's ReloadRules REPLACES the
// whole rule set, wiping the panel's static rules (api inbound, geoip
// private, bittorrent) in one call.
func (x *XrayAPI) AddRoutingRule(ruleTag, userEmail, ip string) error {
	if x.RoutingServiceClient == nil {
		return common.NewError("xray api is not initialized")
	}

	// /32 (or /128 for IPv6) so neighbors behind the same CGNAT stay
	// unaffected; the rule matches user AND source together.
	cidr := ip + "/32"
	if ip6 := net.ParseIP(ip); ip6 != nil && ip6.To4() == nil {
		cidr = ip + "/128"
	}

	// json.Marshal the email so it is escaped safely into the rule JSON —
	// no raw string interpolation of user-controlled values.
	userJSON, err := json.Marshal([]string{userEmail})
	if err != nil {
		return fmt.Errorf("failed to marshal rule user: %w", err)
	}
	sourceJSON, err := json.Marshal([]string{cidr})
	if err != nil {
		return fmt.Errorf("failed to marshal rule source: %w", err)
	}

	ruleJSON := fmt.Sprintf(`{
		"routing": {
			"rules": [
				{
					"ruleTag": %q,
					"user": %s,
					"source": %s,
					"outboundTag": "blocked"
				}
			]
		}
	}`, ruleTag, userJSON, sourceJSON)

	confDecoded, err := confserial.DecodeJSONConfig(strings.NewReader(ruleJSON))
	if err != nil {
		return fmt.Errorf("failed to decode routing rule config: %w", err)
	}
	if confDecoded.RouterConfig == nil {
		return common.NewError("routing rule config did not have routing field")
	}

	routerConf, err := confDecoded.RouterConfig.Build()
	if err != nil {
		return fmt.Errorf("failed to build routing rule: %w", err)
	}

	tmsg := serial.ToTypedMessage(routerConf)
	if tmsg == nil {
		return common.NewError("failed to format routing rule to TypedMessage")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = (*x.RoutingServiceClient).AddRule(ctx, &routerCommand.AddRuleRequest{
		Config:       tmsg,
		ShouldAppend: true,
	})
	if err != nil {
		return fmt.Errorf("failed to add routing rule %s: %w", ruleTag, err)
	}

	return nil
}

// RemoveRoutingRule removes a previously added dynamic routing rule by its
// tag. Removing an unknown tag is a no-op on the Xray side (RemoveRule just
// filters the rule list), so this is safe to call speculatively.
func (x *XrayAPI) RemoveRoutingRule(ruleTag string) error {
	if x.RoutingServiceClient == nil {
		return common.NewError("xray api is not initialized")
	}
	if ruleTag == "" {
		return common.NewError("cannot remove routing rule with empty tag")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := (*x.RoutingServiceClient).RemoveRule(ctx, &routerCommand.RemoveRuleRequest{
		RuleTag: ruleTag,
	})
	if err != nil {
		return fmt.Errorf("failed to remove routing rule %s: %w", ruleTag, err)
	}

	return nil
}

// GetTraffic queries traffic statistics from the Xray core, optionally resetting counters.
func (x *XrayAPI) GetTraffic(reset bool) ([]*Traffic, []*ClientTraffic, error) {
	if x.grpcClient == nil {
		return nil, nil, common.NewError("xray api is not initialized")
	}

	trafficRegex := regexp.MustCompile(`(inbound|outbound)>>>([^>]+)>>>traffic>>>(downlink|uplink)`)
	clientTrafficRegex := regexp.MustCompile(`user>>>([^>]+)>>>traffic>>>(downlink|uplink)`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	if x.StatsServiceClient == nil {
		return nil, nil, common.NewError("xray StatusServiceClient is not initialized")
	}

	resp, err := (*x.StatsServiceClient).QueryStats(ctx, &statsService.QueryStatsRequest{Reset_: reset})
	if err != nil {
		logger.Debug("Failed to query Xray stats:", err)
		return nil, nil, err
	}

	tagTrafficMap := make(map[string]*Traffic)
	emailTrafficMap := make(map[string]*ClientTraffic)

	for _, stat := range resp.GetStat() {
		if matches := trafficRegex.FindStringSubmatch(stat.Name); len(matches) == 4 {
			processTraffic(matches, stat.Value, tagTrafficMap)
		} else if matches := clientTrafficRegex.FindStringSubmatch(stat.Name); len(matches) == 3 {
			processClientTraffic(matches, stat.Value, emailTrafficMap)
		}
	}
	return mapToSlice(tagTrafficMap), mapToSlice(emailTrafficMap), nil
}

// processTraffic aggregates a traffic stat into trafficMap using regex matches and value.
func processTraffic(matches []string, value int64, trafficMap map[string]*Traffic) {
	isInbound := matches[1] == "inbound"
	tag := matches[2]
	isDown := matches[3] == "downlink"

	if tag == "api" {
		return
	}

	traffic, ok := trafficMap[tag]
	if !ok {
		traffic = &Traffic{
			IsInbound:  isInbound,
			IsOutbound: !isInbound,
			Tag:        tag,
		}
		trafficMap[tag] = traffic
	}

	if isDown {
		traffic.Down = value
	} else {
		traffic.Up = value
	}
}

// processClientTraffic updates clientTrafficMap with upload/download values for a client email.
func processClientTraffic(matches []string, value int64, clientTrafficMap map[string]*ClientTraffic) {
	email := matches[1]
	isDown := matches[2] == "downlink"

	traffic, ok := clientTrafficMap[email]
	if !ok {
		traffic = &ClientTraffic{Email: email}
		clientTrafficMap[email] = traffic
	}

	if isDown {
		traffic.Down = value
	} else {
		traffic.Up = value
	}
}

// mapToSlice converts a map of pointers to a slice of pointers.
func mapToSlice[T any](m map[string]*T) []*T {
	result := make([]*T, 0, len(m))
	for _, v := range m {
		result = append(result, v)
	}
	return result
}
