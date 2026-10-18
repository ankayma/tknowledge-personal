// Package ccclient issues ledger_move causes on cause-channel
// (CJ-10): effect_type ledger_move, scope claim:<id>, window 60 s. The returned
// id goes into the row's cause_id; CJCM later matches the two. Shared by the
// CLI (actor tk-cli) and tk-mcp (actor tk-mcp).
package ccclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// WindowSeconds is how long a ledger_move cause stays valid.
const WindowSeconds = 60

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: http.DefaultClient}
}

// LedgerMove is one cause for one move. Approver must be the row's signer (a
// person); Producer who drafted it; Actor the process writing it.
type LedgerMove struct {
	ClaimID  int64
	Approver string
	Producer string
	Actor    string
	Note     string
}

func (c *Client) LedgerMove(ctx context.Context, m LedgerMove) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"effect_type":    "ledger_move",
		"scope":          "claim:" + strconv.FormatInt(m.ClaimID, 10),
		"approver":       m.Approver,
		"producer":       m.Producer,
		"actor":          m.Actor,
		"claims":         []int64{},
		"window_seconds": WindowSeconds,
		"note":           m.Note,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/causes", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("cause-channel: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("cause-channel %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("cause-channel: bad response %q", raw)
	}
	return out.ID, nil
}
