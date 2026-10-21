package localassistant

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"sync"
	"time"
)

// SyntheticClaims exercises the pairing protocol ONLY in an in-process fixture.
// It has no HTTP listener, persistence, device credential, execution authority,
// or integration with RunFile. A receipt cannot replace --approve/session-file.
type SyntheticClaims struct {
	mu      sync.Mutex
	now     func() time.Time
	pending map[string]SyntheticChallenge
}

type SyntheticChallenge struct {
	Schema           string    `json:"schema"`
	ChallengeID      string    `json:"challenge_id"`
	DeviceAlias      string    `json:"device_alias"`
	Origin           string    `json:"origin"`
	JobDigest        string    `json:"job_digest"`
	ConfirmationCode string    `json:"confirmation_code"`
	ExpiresAt        time.Time `json:"expires_at"`
	SyntheticOnly    bool      `json:"synthetic_only"`
}

type SyntheticClaimReceipt struct {
	Schema              string    `json:"schema"`
	ChallengeID         string    `json:"challenge_id"`
	DeviceAlias         string    `json:"device_alias"`
	JobDigest           string    `json:"job_digest"`
	ConfirmedAt         time.Time `json:"confirmed_at"`
	SyntheticOnly       bool      `json:"synthetic_only"`
	AuthorizesExecution bool      `json:"authorizes_execution"`
	CredentialIssued    bool      `json:"credential_issued"`
}

func NewSyntheticClaims(now func() time.Time) *SyntheticClaims {
	if now == nil {
		now = time.Now
	}
	return &SyntheticClaims{now: now, pending: map[string]SyntheticChallenge{}}
}

// Begin accepts only explicit loopback HTTPS fixture origins. It never contacts
// that origin. The short-lived code is synthetic test data, not an account TOTP.
func (s *SyntheticClaims) Begin(origin, deviceAlias, jobDigest string) (SyntheticChallenge, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || !safeID.MatchString(deviceAlias) || !digestPattern.MatchString(jobDigest) {
		return SyntheticChallenge{}, errors.New("assistant: synthetic pairing input rejected")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return SyntheticChallenge{}, errors.New("assistant: synthetic pairing is limited to loopback fixtures")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	for id, c := range s.pending {
		if !now.Before(c.ExpiresAt) {
			delete(s.pending, id)
		}
	}
	if len(s.pending) >= 32 {
		return SyntheticChallenge{}, errors.New("assistant: synthetic pairing fixture is at capacity")
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return SyntheticChallenge{}, errors.New("assistant: synthetic challenge unavailable")
	}
	c := SyntheticChallenge{Schema: "lantai.synthetic-device-challenge/v1", ChallengeID: hex.EncodeToString(random[:16]), ConfirmationCode: hex.EncodeToString(random[16:]), DeviceAlias: deviceAlias, Origin: origin, JobDigest: jobDigest, ExpiresAt: now.Add(2 * time.Minute), SyntheticOnly: true}
	s.pending[c.ChallengeID] = c
	return c, nil
}

// Confirm consumes the challenge on its first confirmation attempt. Successful
// concurrent/replayed attempts therefore cannot mint another receipt.
func (s *SyntheticClaims) Confirm(id, code, deviceAlias, jobDigest string) (SyntheticClaimReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.pending[id]
	delete(s.pending, id)
	now := s.now().UTC()
	if !ok || !now.Before(c.ExpiresAt) || c.DeviceAlias != deviceAlias || c.JobDigest != jobDigest || subtle.ConstantTimeCompare([]byte(c.ConfirmationCode), []byte(code)) != 1 {
		return SyntheticClaimReceipt{}, errors.New("assistant: synthetic challenge absent, expired, consumed, or mismatched")
	}
	return SyntheticClaimReceipt{Schema: "lantai.synthetic-device-claim-receipt/v1", ChallengeID: id, DeviceAlias: c.DeviceAlias, JobDigest: c.JobDigest, ConfirmedAt: now, SyntheticOnly: true}, nil
}
