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

// Consume redeems a reset credit through the Codex app server.
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
	response, err := exchange(ctx, cmd, request)
	if err != nil {
		return "", err
	}
	if response.Result.Outcome == nil || *response.Result.Outcome == "" {
		return "", errors.New("codexlimit: answer has no outcome")
	}
	switch *response.Result.Outcome {
	case OutcomeReset, OutcomeNothingToReset, OutcomeNoCredit, OutcomeAlreadyRedeemed:
		return *response.Result.Outcome, nil
	default:
		return "", fmt.Errorf("codexlimit: unknown outcome %q", *response.Result.Outcome)
	}
}
