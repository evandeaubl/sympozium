package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/go-logr/logr"
)

type triggerKind string

const (
	kindDM      triggerKind = "dm"
	kindMention triggerKind = "mention"
	kindChannel triggerKind = "channel"
)

type matrixConfig struct {
	allowedTriggers map[string]bool
}

var validTriggerKinds = map[string]bool{
	string(kindDM):      true,
	string(kindMention): true,
	string(kindChannel): true,
}

func csvToSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func loadMatrixConfig(log logr.Logger) *matrixConfig {
	cfg := &matrixConfig{
		allowedTriggers: csvToSet(os.Getenv("MATRIX_ALLOWED_TRIGGERS")),
	}
	for v := range cfg.allowedTriggers {
		if !validTriggerKinds[v] {
			log.Info("WARNING: MATRIX_ALLOWED_TRIGGERS contains unknown value; it will never match",
				"value", v, "validValues", []string{string(kindDM), string(kindMention), string(kindChannel)})
		}
	}
	return cfg
}

func (c *matrixConfig) triggerAllowed(k triggerKind) bool {
	if len(c.allowedTriggers) == 0 {
		return true
	}
	return c.allowedTriggers[string(k)]
}

// classifyKind decides whether the message is a DM, an @-mention of the
// bot, or a generic channel/group message.
//
// For Matrix:
//   - DM is determined by the m.direct account data on the user's profile,
//     which maps user IDs to lists of room IDs. We check this via the
//     /account/{userId}/data/m.direct endpoint. As a fallback, rooms with
//     exactly 2 joined members and one of them is the bot are also treated
//     as DMs.
//   - Mention is detected by checking for "@<botLocalpart>:<server>" in the
//     message text. Matrix mentions use the full MXID format.
//   - Otherwise it's a "channel" message.
func classifyKind(text, botUserID, roomID, userID, accessToken, homeserverURL string, client *http.Client) (triggerKind, error) {
	if botUserID == "" {
		return kindChannel, nil
	}

	if isDMRoom(roomID, userID, accessToken, homeserverURL, client) {
		return kindDM, nil
	}

	if isMention(text, botUserID) {
		return kindMention, nil
	}

	return kindChannel, nil
}

// isDMRoom checks if the given room is a DM room. It first checks the
// m.direct account data for the user, then falls back to checking room members.
func isDMRoom(roomID, userID, accessToken, homeserverURL string, client *http.Client) bool {
	if isDMInAccountData(roomID, userID, accessToken, homeserverURL, client) {
		return true
	}
	return isDMByMemberCount(roomID, accessToken, homeserverURL, client)
}

// isDMInAccountData checks the m.direct account data to see if roomID
// is listed as a DM for any user.
func isDMInAccountData(roomID, userID, accessToken, homeserverURL string, client *http.Client) bool {
	if accessToken == "" || homeserverURL == "" {
		return false
	}

	signedUserID := url.PathEscape(userID)
	url := homeserverURL + apiSuffix + "/user/" + signedUserID + "/account_data/m.direct"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var directData map[string]map[string][]string
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &directData); err != nil {
		// m.direct may be in the format map[string][]string (user -> [roomIDs])
		// or map[string]map[string][]string in newer specs.
		var directDataOld map[string][]string
		if err := json.Unmarshal(body, &directDataOld); err != nil {
			return false
		}
		for _, roomIDs := range directDataOld {
			for _, rid := range roomIDs {
				if rid == roomID {
					return true
				}
			}
		}
		return false
	}

	for _, roomIDLists := range directData {
		for _, roomIDs := range roomIDLists {
			for _, rid := range roomIDs {
				if rid == roomID {
					return true
				}
			}
		}
	}
	return false
}

// isDMByMemberCount checks if the room has exactly 2 joined members.
// This serves as a heuristic fallback for rooms that haven't set m.direct.
func isDMByMemberCount(roomID, accessToken, homeserverURL string, client *http.Client) bool {
	if accessToken == "" || homeserverURL == "" {
		return false
	}

	signedRoomID := url.PathEscape(roomID)
	requestURL := homeserverURL + apiSuffix + "/rooms/" + signedRoomID + "/joined_members"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, requestURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var membersResp struct {
		Joined map[string]struct {
			DisplayName string `json:"display_name,omitempty"`
		} `json:"joined"`
	}

	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &membersResp); err != nil {
		return false
	}

	return len(membersResp.Joined) == 2
}

// isMention checks if the bot is mentioned in the message text.
// Matrix mentions typically use the full MXID format like "@bot:matrix.org".
// We also handle "!" prefixed room aliases and bare mentions.
func isMention(text, botUserID string) bool {
	if text == "" || botUserID == "" {
		return false
	}

	lowerText := strings.ToLower(text)
	lowerBotID := strings.ToLower(botUserID)

	// Check for full MXID mention (e.g. "@bot:matrix.org")
	if strings.Contains(lowerText, lowerBotID) {
		return true
	}

	// Check for "Hey <@bot:matrix.org>" style (Slack-like, but some clients)
	if strings.Contains(lowerText, "<@"+lowerBotID+">") {
		return true
	}

	// Extract localpart from botUserID for bare mention matching.
	// e.g. "@mybot:matrix.org" -> "mybot"
	// Only match as a word boundary to avoid false positives.
	localpart := extractDisplayNameFromMXID(botUserID)
	if localpart != "" {
		lowerLocalpart := strings.ToLower(localpart)
		for {
			idx := strings.Index(lowerText, lowerLocalpart)
			if idx == -1 {
				break
			}
			// Check word boundary before.
			beforeOK := idx == 0 || lowerText[idx-1] == '@' ||
				lowerText[idx-1] == ' ' || lowerText[idx-1] == '\t' ||
				lowerText[idx-1] == '\n'
			// Check word boundary after.
			afterIdx := idx + len(lowerLocalpart)
			afterOK := afterIdx == len(lowerText) ||
				lowerText[afterIdx] == ' ' || lowerText[afterIdx] == '\t' ||
				lowerText[afterIdx] == '\n'
			if beforeOK && afterOK {
				return true
			}
			// Advance past this match to check for other occurrences.
			lowerText = lowerText[idx+len(lowerLocalpart):]
		}
	}

	return false
}
