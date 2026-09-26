// Package main is the entry point for the Matrix channel pod.
// Uses the Matrix Client-Server HTTP API directly for login, syncing,
// and sending messages. Supports password-based login (stores credentials
// via Kubernetes secrets) or pre-obtained access tokens.
//
// Configuration:
//   - MATRIX_HOMESERVER: Base URL of the Matrix homeserver (e.g. https://matrix.org)
//   - MATRIX_USER_ID: MXID of the bot account (e.g. @mybot:matrix.org)
//   - MATRIX_PASSWORD: Password for the bot account (SENSITIVE — via env var / secret)
//   - MATRIX_ACCESS_TOKEN: Pre-obtained access token (alternative to password; SENSITIVE)
//   - INSTANCE_NAME: Agent name this channel is bound to
//   - EVENT_BUS_URL: NATS event bus URL
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"github.com/sympozium-ai/sympozium/internal/channel"
	"github.com/sympozium-ai/sympozium/internal/eventbus"
)

const (
	apiSuffix      = "/_matrix/client/v3"
	defaultTimeout = 30 * time.Second
	syncTimeout    = 60 * time.Second
	reconnectWait  = 5 * time.Second
	txnIDWindow    = 60 * 60 * 1000 // 1 hour wrap for transaction IDs
)

// MatrixChannel implements the Matrix channel using the Client-Server HTTP API.
type MatrixChannel struct {
	channel.BaseChannel
	homeserverURL string
	accessToken   string
	userID        string
	client        *http.Client
	healthy       bool
	mu            sync.RWMutex
}

// matrixSyncResponse is a subset of the Matrix /sync response.
type matrixSyncResponse struct {
	NextBatch string          `json:"next_batch"`
	Rooms     matrixSyncRooms `json:"rooms"`
}

// matrixSyncRooms maps membership type (join, leave, invite) to room data.
type matrixSyncRooms struct {
	Join   map[string]matrixSyncJoinedRoom `json:"join"`
	Leave  map[string]matrixSyncLeftRoom   `json:"leave"`
	Invite map[string]matrixSyncInvitedRoom `json:"invite"`
}

type matrixSyncJoinedRoom struct {
	Timeline matrixTimeline `json:"timeline"`
}

type matrixSyncLeftRoom struct {
	Timeline matrixTimeline `json:"timeline"`
}

type matrixSyncInvitedRoom struct {
	InviteState struct {
		Events []matrixSyncEvent `json:"events"`
	} `json:"invite_state"`
}

type matrixTimeline struct {
	Events  []matrixSyncEvent `json:"events"`
	Limited bool              `json:"limited"`
}

// matrixSyncEvent is a single event from the /sync timeline.
// We only care about m.room.message events.
type matrixSyncEvent struct {
	EventID        string         `json:"event_id"`
	Type           string         `json:"type"`
	Sender         string         `json:"sender"`
	OriginServerTS int64          `json:"origin_server_ts"`
	Content        map[string]any `json:"content"`
}

// loginResponse is the response from /login.
type loginResponse struct {
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
	UserID      string `json:"user_id"`
	ErrCode     string `json:"errcode,omitempty"`
	Error       string `json:"error,omitempty"`
}

// sendResponse is the response from /rooms/{roomId}/send/{eventType}/{txnId}.
type sendResponse struct {
	EventID string `json:"event_id"`
	ErrCode string `json:"errcode,omitempty"`
	Error   string `json:"error,omitempty"`
}

func main() {
	var instanceName string
	var eventBusURL string
	var homeserverURL string
	var userID string
	var password string
	var accessToken string
	var listenAddr string

	flag.StringVar(&instanceName, "instance", os.Getenv("INSTANCE_NAME"), "Agent name")
	flag.StringVar(&eventBusURL, "event-bus-url", os.Getenv("EVENT_BUS_URL"), "Event bus URL")
	flag.StringVar(&homeserverURL, "homeserver-url", os.Getenv("MATRIX_HOMESERVER"), "Matrix homeserver base URL (e.g. https://matrix.org)")
	flag.StringVar(&userID, "user-id", os.Getenv("MATRIX_USER_ID"), "Matrix bot user ID (e.g. @mybot:matrix.org)")
	flag.StringVar(&password, "password", os.Getenv("MATRIX_PASSWORD"), "Matrix bot password (SENSITIVE)")
	flag.StringVar(&accessToken, "access-token", os.Getenv("MATRIX_ACCESS_TOKEN"), "Matrix access token (alternative to password; SENSITIVE)")
	flag.StringVar(&listenAddr, "addr", ":8080", "Listen address for health endpoint")
	flag.Parse()

	if homeserverURL == "" {
		fmt.Fprintln(os.Stderr, "MATRIX_HOMESERVER is required (e.g. https://matrix.org)")
		os.Exit(1)
	}
	homeserverURL = strings.TrimRight(homeserverURL, "/")

	log := zap.New(zap.UseDevMode(false)).WithName("channel-matrix")

	bus, err := eventbus.NewNATSEventBus(eventBusURL)
	if err != nil {
		log.Error(err, "failed to connect to event bus")
		os.Exit(1)
	}
	defer bus.Close()

	// Determine access token: either provided directly or obtained via login.
	resolvedToken := accessToken
	if resolvedToken == "" {
		if userID == "" || password == "" {
			fmt.Fprintln(os.Stderr, "MATRIX_ACCESS_TOKEN or (MATRIX_USER_ID + MATRIX_PASSWORD) is required")
			os.Exit(1)
		}
		resolvedToken, err = loginMatrix(homeserverURL, userID, password)
		if err != nil {
			log.Error(err, "failed to login to Matrix")
			os.Exit(1)
		}
		log.Info("Logged in to Matrix", "userId", userID)
	}

	mc := &MatrixChannel{
		BaseChannel: channel.BaseChannel{
			ChannelType:  "matrix",
			InstanceName: instanceName,
			EventBus:     bus,
		},
		homeserverURL: homeserverURL,
		accessToken:   resolvedToken,
		userID:        userID,
		client:        &http.Client{Timeout: defaultTimeout},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Set presence to available during normal shutdown.
	go func() {
		<-ctx.Done()
		_ = mc.setPresence(context.Background(), "unavailable")
	}()

	// Verify we can reach the homeserver and the token is valid.
	if err := mc.verifyConnection(ctx); err != nil {
		log.Error(err, "matrix connection check failed")
		_ = mc.PublishHealth(ctx, channel.HealthStatus{Connected: false, Message: err.Error()})
	} else {
		// Set presence to online after successful connection.
		if userID != "" {
			if err := mc.setPresence(ctx, "online"); err != nil {
				log.Error(err, "failed to set presence to online")
			} else {
				log.Info("Presence set to online", "userId", userID)
			}
		}
	}

	// Start health server.
	go mc.startHealthServer(ctx, listenAddr)

	// Start outbound message handler.
	go mc.handleOutbound(ctx)

	log.Info("Starting Matrix channel", "instance", instanceName, "homeserver", homeserverURL)

	if err := mc.syncLoop(ctx); err != nil {
		log.Error(err, "matrix sync loop failed")
	}
}

// verifyConnection checks that the access token is valid by calling /account/whoami.
func (mc *MatrixChannel) verifyConnection(ctx context.Context) error {
	resp, err := mc.doMatrixRequest(ctx, http.MethodGet, "/account/whoami", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("whoami returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (mc *MatrixChannel) setPresence(ctx context.Context, presence string) error {
	path := "/presence/" + url.PathEscape(mc.userID) + "/status"
	body := map[string]string{"presence": presence}
	bodyBytes, _ := json.Marshal(body)
	resp, err := mc.doMatrixRequest(ctx, http.MethodPut, path, bodyBytes)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("presence update failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// loginMatrix performs a password-based login and returns the access token.
func loginMatrix(homeserverURL, userID, password string) (string, error) {
	payload := map[string]any{
		"type": "m.login.password",
		"identifier": map[string]any{
			"type": "m.id.user",
			"user": userID,
		},
		"password": password,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	resp, err := http.Post(
		homeserverURL+apiSuffix+"/login",
		"application/json",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("login failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	var loginResp loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return "", fmt.Errorf("decoding login response: %w", err)
	}
	if loginResp.ErrCode != "" {
		return "", fmt.Errorf("login error (%s): %s", loginResp.ErrCode, loginResp.Error)
	}
	return loginResp.AccessToken, nil
}

// syncLoop continuously calls /sync with long-polling to receive messages.
func (mc *MatrixChannel) syncLoop(ctx context.Context) error {
	syncURL := mc.homeserverURL + apiSuffix + "/sync"
	since := ""
	mc.setHealthy(true, "")
	// On the first sync (no since token), we skip processing messages to avoid
	// re-processing messages from before the restart.
	hasSynced := false

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		params := url.Values{}
		params.Set("timeout", strconv.Itoa(int(syncTimeout.Milliseconds())))
		params.Set("full_state", "false")
		if since != "" {
			params.Set("since", since)
		}

		fullURL := syncURL + "?" + params.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			mc.setHealthy(false, "failed to build sync request")
			time.Sleep(reconnectWait)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+mc.accessToken)

		resp, err := mc.client.Do(req)
		if err != nil {
			mc.setHealthy(false, mc.redactError(err.Error()))
			time.Sleep(reconnectWait)
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			mc.setHealthy(false, "access token rejected (401)")
			_ = mc.PublishHealth(ctx, channel.HealthStatus{Connected: false, Message: "access token rejected"})
			return fmt.Errorf("access token rejected by Matrix server (401)")
		}

		if resp.StatusCode != http.StatusOK {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			mc.setHealthy(false, fmt.Sprintf("sync returned HTTP %d", resp.StatusCode))
			time.Sleep(reconnectWait)
			continue
		}

		var syncResp matrixSyncResponse
		if err := json.NewDecoder(resp.Body).Decode(&syncResp); err != nil {
			resp.Body.Close()
			mc.setHealthy(false, "failed to decode sync response")
			time.Sleep(reconnectWait)
			continue
		}
		resp.Body.Close()

		mc.setHealthy(true, "")

		// Process invited rooms - auto-accept invitations.
		for roomID := range syncResp.Rooms.Invite {
			fmt.Fprintf(os.Stderr, "Auto-accepting invitation to room %s\n", roomID)
			if err := mc.joinRoom(ctx, roomID); err != nil {
				fmt.Fprintf(os.Stderr, "failed to auto-join room %s: %v\n", roomID, err)
			}
		}

		// Process joined room events - skip on first sync to avoid re-processing
		// messages from before the restart.
		if hasSynced {
			for roomID, roomData := range syncResp.Rooms.Join {
				for _, event := range roomData.Timeline.Events {
					if event.Type == "m.room.message" {
						mc.handleSyncEvent(ctx, roomID, event)
					}
				}
			}
		}

		if since == "" {
			fmt.Fprintln(os.Stderr, "Initial sync complete, ready to process new messages")
		}
		hasSynced = true
		since = syncResp.NextBatch
	}
}

// joinRoom accepts an invitation and joins a room.
func (mc *MatrixChannel) joinRoom(ctx context.Context, roomID string) error {
	signedID := url.PathEscape(roomID)
	path := "/rooms/" + signedID + "/join"
	resp, err := mc.doMatrixRequest(ctx, http.MethodPost, path, []byte("{}"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to join room %s (HTTP %d): %s", roomID, resp.StatusCode, string(body))
	}
	return nil
}

// handleSyncEvent processes a single m.room.message event from /sync.
func (mc *MatrixChannel) handleSyncEvent(ctx context.Context, roomID string, event matrixSyncEvent) {
	if event.Sender == "" {
		return
	}
	// Ignore messages sent by the bot itself to prevent infinite loops.
	if event.Sender == mc.userID {
		return
	}

	text, _ := event.Content["body"].(string)
	msgtype, _ := event.Content["msgtype"].(string)

	// Only process text messages (m.text, m.notice, m.emote).
	// For m.notice, we still forward to allow agents to read bot notices.
	if msgtype != "" && msgtype != "m.text" && msgtype != "m.notice" && msgtype != "m.emote" {
		return
	}
	if text == "" {
		return
	}

	senderID := event.Sender
	displayName := extractDisplayNameFromMXID(senderID)

	msg := channel.InboundMessage{
		SenderID:   senderID,
		SenderName: displayName,
		ChatID:     roomID,
		Text:       text,
		Metadata: map[string]string{
			"messageId": event.EventID,
			"msgtype":   msgtype,
			"originTs":  strconv.FormatInt(event.OriginServerTS, 10),
		},
	}

	if err := mc.PublishInbound(ctx, msg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to publish inbound: %v\n", err)
	}
}

// handleOutbound subscribes to outbound messages and sends them via Matrix.
func (mc *MatrixChannel) handleOutbound(ctx context.Context) {
	events, err := mc.SubscribeOutbound(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to subscribe to outbound: %v\n", err)
		return
	}

	var txnMu sync.Mutex
	var lastTxnID int64

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			var msg channel.OutboundMessage
			if err := json.Unmarshal(event.Data, &msg); err != nil {
				continue
			}
			if msg.Channel != "matrix" {
				continue
			}
			if err := mc.sendMessage(ctx, msg, &txnMu, &lastTxnID); err != nil {
				fmt.Fprintf(os.Stderr, "failed to send matrix message: %v\n", err)
			}
		}
	}
}

// markdownConverter converts Markdown text to HTML using goldmark.
// It supports common extensions like tables, strikethrough, task lists,
// and GFM (GitHub Flavored Markdown) autolinks.
var markdownConverter = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// convertMarkdownToHTML converts Markdown text to HTML. If conversion
// fails for any reason, it returns an error so the caller can fall back
// to sending plain text.
func convertMarkdownToHTML(markdown string) (string, error) {
	var buf strings.Builder
	if err := markdownConverter.Convert([]byte(markdown), &buf); err != nil {
		return "", err
	}
	html := strings.TrimSpace(buf.String())
	if html == "" {
		return "", fmt.Errorf("empty HTML output")
	}
	return html, nil
}

// sendMessage sends a message to a Matrix room via PUT /rooms/{roomId}/send/m.room.message/{txnId}.
// Message format handling:
//   - "html": the text is used directly as formatted_body with org.matrix.custom.html
//   - "markdown": the text is converted to HTML and attached as formatted_body
//   - "plain" or any other value: sent as plain text without formatted_body
func (mc *MatrixChannel) sendMessage(ctx context.Context, msg channel.OutboundMessage, txnMu *sync.Mutex, lastTxnID *int64) error {
	txnID := nextTxnID(txnMu, lastTxnID)

	content := map[string]any{
		"msgtype": "m.text",
		"body":    msg.Text,
	}

	switch msg.Format {
	case "html":
		content["format"] = "org.matrix.custom.html"
		content["formatted_body"] = msg.Text
	case "markdown":
		if html, err := convertMarkdownToHTML(msg.Text); err == nil {
			content["format"] = "org.matrix.custom.html"
			content["formatted_body"] = html
		}
	case "plain", "":
		// Plain text — no formatted_body.
	default:
		// Unknown format value — log and send as plain text.
		fmt.Fprintf(os.Stderr, "unknown message format %q, sending as plain text\n", msg.Format)
	}

	// Handle reply (reply_to in metadata or ReplyTo field).
	if msg.ReplyTo != "" {
		content["m.relates_to"] = map[string]any{
			"m.in_reply_to": map[string]any{
				"event_id": msg.ReplyTo,
			},
		}
	}

	body, err := json.Marshal(content)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/rooms/%s/send/m.room.message/%d", msg.ChatID, txnID)
	resp, err := mc.doMatrixRequest(ctx, http.MethodPut, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send message failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	respBody, _ := io.ReadAll(resp.Body)
	var sendResp sendResponse
	_ = json.Unmarshal(respBody, &sendResp)
	if sendResp.ErrCode != "" {
		return fmt.Errorf("matrix error (%s): %s", sendResp.ErrCode, sendResp.Error)
	}

	return nil
}

// nextTxnID generates a monotonically increasing, unique transaction ID.
func nextTxnID(txnMu *sync.Mutex, lastTxnID *int64) int64 {
	txnMu.Lock()
	defer txnMu.Unlock()
	*lastTxnID = (*lastTxnID + 1) % txnIDWindow
	if *lastTxnID == 0 {
		*lastTxnID = 1
	}
	return *lastTxnID
}

// doMatrixRequest makes an authenticated HTTP request to the Matrix API.
func (mc *MatrixChannel) doMatrixRequest(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequestWithContext(ctx, method, mc.homeserverURL+apiSuffix+path, strings.NewReader(string(body)))
	} else {
		req, err = http.NewRequestWithContext(ctx, method, mc.homeserverURL+apiSuffix+path, nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+mc.accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return mc.client.Do(req)
}

// startHealthServer runs the health endpoint.
func (mc *MatrixChannel) startHealthServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		mc.mu.RLock()
		h := mc.healthy
		mc.mu.RUnlock()
		if h {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = server.Shutdown(shutdownCtx)
	}()

	_ = server.ListenAndServe()
}

// setHealthy updates the health status and publishes it to the event bus.
func (mc *MatrixChannel) setHealthy(connected bool, message string) {
	mc.mu.Lock()
	mc.healthy = connected
	mc.mu.Unlock()
	_ = mc.PublishHealth(context.Background(), channel.HealthStatus{
		Connected: connected,
		Message:   message,
	})
}

// redactError replaces the access token wherever it appears in an error string.
func (mc *MatrixChannel) redactError(s string) string {
	if mc.accessToken == "" {
		return s
	}
	return strings.ReplaceAll(s, mc.accessToken, "***REDACTED***")
}

// extractDisplayNameFromMXID extracts a readable name from a Matrix user ID.
// e.g. "@mybot:matrix.org" -> "mybot"
func extractDisplayNameFromMXID(mxid string) string {
	parts := strings.Split(mxid, ":")
	if len(parts) >= 1 && strings.HasPrefix(parts[0], "@") {
		return strings.TrimPrefix(parts[0], "@")
	}
	return mxid
}
