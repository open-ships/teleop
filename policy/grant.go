package policy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/open-ships/teleop"
)

type Scope struct {
	Command  string `json:"command"`
	Actuator string `json:"actuator"`
	Mode     string `json:"mode"`
}
type Grant struct {
	ID        string           `json:"id"`
	Subject   string           `json:"subject"`
	Session   teleop.SessionID `json:"session"`
	NotBefore time.Time        `json:"not_before"`
	ExpiresAt time.Time        `json:"expires_at"`
	Scopes    []Scope          `json:"scopes"`
}
type SignedGrant struct {
	Grant     Grant  `json:"grant"`
	Signature []byte `json:"signature"`
}

func grantBytes(grant Grant) ([]byte, error) {
	if grant.ID == "" || grant.Subject == "" || grant.Session == (teleop.SessionID{}) || grant.NotBefore.IsZero() || !grant.ExpiresAt.After(grant.NotBefore) || len(grant.Scopes) == 0 || len(grant.Scopes) > 256 {
		return nil, errors.New("policy: incomplete grant")
	}
	for _, scope := range grant.Scopes {
		if scope.Command == "" || scope.Actuator == "" || scope.Mode == "" {
			return nil, errors.New("policy: incomplete scope")
		}
	}
	encoded, err := json.Marshal(grant)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maximumPayload {
		return nil, errors.New("policy: grant too large")
	}
	return append([]byte("teleop/operator-grant/v1\x00"), encoded...), nil
}

// SignGrant belongs in the independent authorization service, not in the live
// controller. Short-lived grants bound revocation lag; online revocation and
// key rotation are deployment services, not claims made by this offline token.
func SignGrant(private ed25519.PrivateKey, grant Grant) (SignedGrant, error) {
	if len(private) != ed25519.PrivateKeySize {
		return SignedGrant{}, errors.New("policy: invalid signing key")
	}
	encoded, err := grantBytes(grant)
	if err != nil {
		return SignedGrant{}, err
	}
	grant.Scopes = slices.Clone(grant.Scopes)
	return SignedGrant{Grant: grant, Signature: ed25519.Sign(private, encoded)}, nil
}

type grantContextKey struct{}

// GrantFromContext returns an isolated credential for an authenticated
// transport envelope. Extraction does not verify or authorize the credential.
func GrantFromContext(ctx context.Context) (SignedGrant, bool) {
	grant, ok := ctx.Value(grantContextKey{}).(SignedGrant)
	grant.Signature = slices.Clone(grant.Signature)
	grant.Grant.Scopes = slices.Clone(grant.Grant.Scopes)
	return grant, ok
}

// WithGrant carries untrusted credentials; Evaluate verifies them every time.
func WithGrant(ctx context.Context, grant SignedGrant) context.Context {
	grant.Signature = slices.Clone(grant.Signature)
	grant.Grant.Scopes = slices.Clone(grant.Grant.Scopes)
	return context.WithValue(ctx, grantContextKey{}, grant)
}

func (policy *Policy) verifyGrant(signed SignedGrant, session teleop.SessionID, now time.Time, scope Scope) error {
	encoded, err := grantBytes(signed.Grant)
	if err != nil {
		return err
	}
	grant := signed.Grant
	if !ed25519.Verify(policy.config.AuthorityKey, encoded, signed.Signature) {
		return errors.New("operator grant signature invalid")
	}
	if grant.Session != session || now.Before(grant.NotBefore) || !now.Before(grant.ExpiresAt) || grant.ExpiresAt.Sub(grant.NotBefore) > policy.config.MaximumGrantTTL || !slices.Contains(grant.Scopes, scope) {
		return errors.New("operator grant outside session, time or scope")
	}
	return nil
}
