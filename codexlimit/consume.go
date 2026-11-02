package codexlimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
)

// Outcome describes the result of consuming a reset credit.
type Outcome string

const (
	OutcomeReset           Outcome = "reset"
	OutcomeNothingToReset  Outcome = "nothingToReset"
	OutcomeNoCredit        Outcome = "noCredit"
	OutcomeAlreadyRedeemed Outcome = "alreadyRedeemed"
)

// Consume redeems a reset credit through the Codex app server. An ended context
// never starts the server. After a context error the request may still have been
// served, so a retry passes the same idempotency key: a spent credit then answers
// OutcomeAlreadyRedeemed instead of spending another.
func Consume(ctx context.Context, cmd *exec.Cmd, idempotencyKey, creditID string) (Outcome, error) {
	if idempotencyKey == "" {
		return "", errors.New("codexlimit: idempotency key is empty")
	}
	params := map[string]string{"idempotencyKey": idempotencyKey}
	if creditID != "" {
		params["creditId"] = creditID
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("codexlimit: encode consume request: %w", err)
	}
	request := fmt.Sprintf("{\"id\":2,\"method\":\"account/rateLimitResetCredit/consume\",\"params\":%s}", encoded)
	raw, err := exchange(ctx, cmd, request)
	if err != nil {
		return "", err
	}
	var result struct {
		Outcome Outcome `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("codexlimit: decode consume result: %w", err)
	}
	switch result.Outcome {
	case "":
		return "", errors.New("codexlimit: answer has no outcome")
	case OutcomeReset, OutcomeNothingToReset, OutcomeNoCredit, OutcomeAlreadyRedeemed:
		return result.Outcome, nil
	default:
		return "", fmt.Errorf("codexlimit: unknown outcome %q", result.Outcome)
	}
}
