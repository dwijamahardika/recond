package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Webhook signing contract (the same scheme ledgerd uses for its outbound
// webhooks, so the two services speak one dialect):
//
//	Webhook-Timestamp: <unix seconds>
//	Webhook-Signature: v1=<hex HMAC-SHA256(secret, timestamp + "." + raw body)>
//
// Several comma-separated v1 signatures are accepted so a provider can sign
// with old and new secrets during rotation.
const (
	HeaderSignature = "Webhook-Signature"
	HeaderTimestamp = "Webhook-Timestamp"
)

// Verification failures. Callers map all of them to 401.
var (
	ErrBadSignature = errors.New("signature mismatch")
	ErrStale        = errors.New("timestamp outside tolerance")
	ErrNoSignature  = errors.New("missing signature or timestamp")
)

// Sign computes the v1 signature.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks freshness first (cheap, and a replay outside the window is
// rejected no matter how valid its MAC), then compares every presented
// signature with hmac.Equal - constant time, so a forger learns nothing from
// response timing. Replays inside the window are harmless: event ids are
// idempotent.
func Verify(secret []byte, timestamp, header string, body []byte, tolerance time.Duration, now time.Time) error {
	if timestamp == "" || header == "" {
		return ErrNoSignature
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrNoSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return ErrStale
	}
	want := []byte(Sign(secret, timestamp, body))
	ok := false
	for _, sig := range strings.Split(header, ",") {
		if hmac.Equal(want, []byte(strings.TrimSpace(sig))) {
			ok = true // keep looping: no early exit on which signature matched
		}
	}
	if !ok {
		return ErrBadSignature
	}
	return nil
}
